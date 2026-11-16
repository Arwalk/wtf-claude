package report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"claude-usage/internal/api"
)

var now = time.Date(2026, 11, 16, 16, 47, 48, 0, time.UTC)

func fixtureUsage(t *testing.T) *api.Usage {
	t.Helper()
	b, err := os.ReadFile("../../testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	u, err := api.ParseUsage(b)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLimitLabels(t *testing.T) {
	cases := []struct{ kind, model, surface, want string }{
		{"weekly_all", "", "", "Weekly · all models"},
		{"weekly_scoped", "Fable", "", "Weekly · Fable"},
		{"weekly_scoped", "", "claude_code", "Weekly · Claude code"},
		{"weekly_scoped", "", "", "Weekly · scoped"},
		{"five_hour", "", "", "Session (5h)"},
		{"session", "", "", "Session (5h)"},
		{"session_all", "", "", "Session (5h)"},
		{"seven_day", "", "", "Weekly · all models"},
		{"seven_day_opus", "", "", "Weekly · Opus"},
		{"seven_day_oauth_apps", "", "", "Weekly · OAuth Apps"},
		{"monthly_moonbeam", "", "", "Monthly moonbeam"},
		{"monthly_moonbeam", "Opus", "", "Monthly moonbeam · Opus"},
		{"", "", "", "Limit"},
	}
	for _, c := range cases {
		if got := LimitLabel(c.kind, c.model, c.surface); got != c.want {
			t.Errorf("LimitLabel(%q,%q,%q) = %q, want %q", c.kind, c.model, c.surface, got, c.want)
		}
	}
}

func TestBuildFromFixture(t *testing.T) {
	r := Build(Input{Now: now, Usage: fixtureUsage(t)})
	if len(r.Limits) != 2 {
		t.Fatalf("limits = %d", len(r.Limits))
	}
	all, fable := r.Limits[0], r.Limits[1]
	if *all.PercentLeft != 55 || all.ShortLabel != "All" || all.Severity != SeverityOK || all.Active || all.Source != "limits" {
		t.Errorf("all = %+v", all)
	}
	if fable.Label != "Weekly · Fable" || *fable.PercentLeft != 31 || fable.ShortLabel != "Fable" || !fable.Active {
		t.Errorf("fable = %+v", fable)
	}
	// Window from seven_day_breakdown.window_started_at.
	wantStart := time.Date(2026, 11, 15, 10, 0, 0, 495934000, time.UTC)
	if all.WindowStart == nil || !all.WindowStart.Equal(wantStart) || all.WindowSeconds != 7*24*3600 {
		t.Errorf("window = %v %d", all.WindowStart, all.WindowSeconds)
	}
	p := fable.Pace
	if p == nil {
		t.Fatal("no pace")
	}
	// ~30.8h of 168h elapsed => ~18.3% expected; 69% used => well over pace.
	if p.ExpectedPercent < 18 || p.ExpectedPercent > 18.6 || p.Status != PaceOver || p.DeltaPercent < 50 {
		t.Errorf("pace = %+v", p)
	}
	if p.RunsOutAt == nil {
		t.Fatal("expected run-out projection")
	}
	// start + elapsed*100/69 ≈ Nov 17 06:3x UTC
	if p.RunsOutAt.UTC().Format("Jan 2 15") != "Nov 17 06" {
		t.Errorf("runs out at %v", p.RunsOutAt)
	}
	if r.Breakdown == nil || len(r.Breakdown.Rows) != 2 || r.Breakdown.Rows[0].Name != "Claude Code" || r.Breakdown.Rows[1].Name != "Cowork" {
		t.Errorf("breakdown = %+v", r.Breakdown)
	}
	if r.Spend != nil {
		t.Errorf("spend should be hidden when disabled: %+v", r.Spend)
	}
	if ws := WeekStart(fixtureUsage(t), now); !ws.Equal(wantStart) {
		t.Errorf("WeekStart = %v", ws)
	}
}

func TestSeverity(t *testing.T) {
	cases := []struct {
		pct  float64
		api  string
		want string
	}{
		{10, "normal", SeverityOK}, {69, "", SeverityOK}, {70, "", SeverityWarning}, {89, "normal", SeverityWarning},
		{90, "", SeverityCritical}, {10, "warning", SeverityWarning}, {95, "warning", SeverityCritical}, {5, "critical", SeverityCritical},
		// Fractions: the colour follows the "% left" that is displayed.
		{69.5, "", SeverityWarning}, {89.5, "", SeverityCritical}, {99.6, "", SeverityCritical},
	}
	for _, c := range cases {
		if got := severityFor(c.pct, c.api); got != c.want {
			t.Errorf("severityFor(%v,%q) = %q, want %q", c.pct, c.api, got, c.want)
		}
	}
}

func TestPace(t *testing.T) {
	start := now.Add(-50 * time.Hour)
	end := start.Add(100 * time.Hour) // half elapsed
	if p := computePace(50, start, end, now); p.Status != PaceOnTrack || p.RunsOutAt != nil || *p.ProjectedPercent != 100 {
		t.Errorf("on track: %+v", p)
	}
	if p := computePace(20, start, end, now); p.Status != PaceUnder || p.DeltaPercent != -30 || p.RunsOutAt != nil {
		t.Errorf("under: %+v", p)
	}
	p := computePace(80, start, end, now)
	if p.Status != PaceOver || p.RunsOutAt == nil {
		t.Fatalf("over: %+v", p)
	}
	// 80% in 50h => 100% at 62.5h after start.
	if got := p.RunsOutAt.Sub(start); got < 62*time.Hour || got > 63*time.Hour {
		t.Errorf("runs out after %v", got)
	}
	if p := computePace(100, start, end, now); p.RunsOutAt == nil || !p.RunsOutAt.Equal(now) {
		t.Errorf("exhausted: %+v", p)
	}
	// Too early in the window to project.
	if p := computePace(10, now.Add(-time.Hour), now.Add(167*time.Hour), now); p.ProjectedPercent != nil || p.RunsOutAt != nil {
		t.Errorf("early: %+v", p)
	}
	if computePace(10, start, end, end.Add(time.Minute)) != nil {
		t.Error("pace after reset should be nil")
	}
}

func TestLegacyFallback(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"limits":[],
		"five_hour":{"utilization":92,"resets_at":"2026-11-16T18:00:00Z"},
		"seven_day":{"utilization":40,"resets_at":"2026-11-20T10:00:00Z"},
		"seven_day_opus":{"utilization":10,"resets_at":null}}`))
	ls := BuildLimits(u, now)
	if len(ls) != 3 {
		t.Fatalf("got %d limits", len(ls))
	}
	if ls[0].Label != "Session (5h)" || ls[0].Severity != SeverityCritical || !ls[0].Active || ls[0].Source != "legacy" {
		t.Errorf("five_hour = %+v", ls[0])
	}
	if ls[0].WindowSeconds != 5*3600 || ls[0].Pace == nil {
		t.Errorf("five_hour pace missing: %+v", ls[0])
	}
	if ls[1].Label != "Weekly · all models" || ls[2].Label != "Weekly · Opus" || ls[2].ResetsAt != nil || ls[2].Pace != nil {
		t.Errorf("weekly = %+v / %+v", ls[1], ls[2])
	}
	// limits present => legacy ignored
	if got := BuildLimits(fixtureUsage(t), now); got[0].Source != "limits" || len(got) != 2 {
		t.Errorf("legacy used despite limits: %+v", got)
	}
	// nothing at all
	empty, _ := api.ParseUsage([]byte(`{}`))
	if r := Build(Input{Now: now, Usage: empty}); len(r.Limits) != 0 || !r.Status.NoLimitData {
		t.Errorf("empty: %+v", r)
	}
}

func TestUnknownKindAndBadValues(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"limits":[
		{"kind":"monthly_moonbeam","percent":140,"severity":"exceeded","resets_at":"2026-12-01T00:00:00Z"},
		{"kind":"weekly_all","percent":null}]}`))
	ls := BuildLimits(u, now)
	if ls[0].Label != "Monthly moonbeam" || *ls[0].PercentLeft != 0 || ls[0].Severity != SeverityCritical || ls[0].Pace != nil {
		t.Errorf("unknown = %+v", ls[0])
	}
	if ls[1].PercentUsed != nil || ls[1].PercentLeft != nil || ls[1].ResetsAt != nil || ls[1].Severity != SeverityUnknown {
		t.Errorf("null percent = %+v", ls[1])
	}
}

// A null or unreadable percent must stay unknown, not become "0% used":
// no pace, no threshold colour, and no percent in JSON.
func TestUnknownPercentHasNoPace(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"limits":[
		{"kind":"weekly_all","percent":null,"resets_at":"2026-11-22T10:00:00.000000+00:00"},
		{"kind":"weekly_scoped","percent":"n/a","severity":"warning","resets_at":"2026-11-22T10:00:00Z"}]}`))
	ls := BuildLimits(u, now)
	for _, l := range ls {
		if _, ok := l.Used(); ok || l.Pace != nil || l.WindowStart == nil {
			t.Errorf("%s: %+v", l.Kind, l)
		}
	}
	if ls[0].Severity != SeverityUnknown || ls[1].Severity != SeverityWarning {
		t.Errorf("severities %q %q", ls[0].Severity, ls[1].Severity)
	}
	b, _ := json.Marshal(ls[0])
	if strings.Contains(string(b), "percent_used") || strings.Contains(string(b), "percent_left") {
		t.Errorf("json = %s", b)
	}
}

// A limit whose reset time has passed (stale cache across a reset) is
// marked expired and its old percentage is dropped.
func TestExpiredLimit(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"limits":[
		{"kind":"five_hour","percent":30,"resets_at":"2026-11-16T15:47:48Z"},
		{"kind":"weekly_all","percent":95,"severity":"critical","resets_at":"2026-11-15T10:00:00Z"},
		{"kind":"weekly_scoped","percent":20,"resets_at":"2026-11-20T10:00:00Z"}]}`))
	ls := BuildLimits(u, now)
	for _, l := range ls[:2] {
		if !l.Expired || l.PercentUsed != nil || l.PercentLeft != nil || l.Pace != nil || l.Severity != SeverityUnknown || *l.ResetsInSeconds != 0 {
			t.Errorf("%s: %+v", l.Kind, l)
		}
	}
	if ls[2].Expired || ls[2].PercentUsed == nil {
		t.Errorf("current limit: %+v", ls[2])
	}
}

func TestDisplayLeft(t *testing.T) {
	cases := map[float64]float64{0: 100, 0.4: 99, 30: 70, 69.5: 30, 89.5: 10, 99.6: 0, 100: 0, 140: 0}
	for used, want := range cases {
		if got := DisplayLeft(used); got != want {
			t.Errorf("DisplayLeft(%v) = %v, want %v", used, got, want)
		}
	}
}

// A stale response describing a weekly window that has already ended must
// not be used as the local-stats week (it would start before the 7 days
// that are scanned).
func TestWeekStartIgnoresEndedWindow(t *testing.T) {
	ws := now.Add(-(7*24 + 5) * time.Hour)
	body := fmt.Sprintf(`{"seven_day_breakdown":{"window_started_at":%q},
		"limits":[{"kind":"weekly_all","percent":50,"resets_at":%q}]}`,
		ws.Format(time.RFC3339), ws.Add(7*24*time.Hour).Format(time.RFC3339))
	u, _ := api.ParseUsage([]byte(body))
	if got := WeekStart(u, now); !got.IsZero() {
		t.Errorf("WeekStart = %v (now-%v), want zero", got, now.Sub(got))
	}
	// Current window: accepted, and always within the last 7 days.
	ws = now.Add(-30 * time.Hour)
	u.Breakdown.WindowStartedAt = ws
	if got := WeekStart(u, now); !got.Equal(ws) {
		t.Errorf("WeekStart = %v", got)
	}
}

func TestShortLabels(t *testing.T) {
	cases := []struct{ kind, model, surface, want string }{
		{"weekly_all", "", "", "All"},
		{"weekly_scoped", "Fable", "", "Fable"},
		{"weekly_scoped", "", "claude_code", "Claude code"},
		{"five_hour", "", "", "5h"},
		{"session_scoped", "Opus", "", "5h Opus"},
		{"seven_day", "", "", "All"},
		{"seven_day_opus", "", "", "Opus"},
		{"monthly_moonbeam", "", "", "Monthly moonbeam"},
	}
	for _, c := range cases {
		if got := ShortLabel(c.kind, c.model, c.surface); got != c.want {
			t.Errorf("ShortLabel(%q,%q,%q) = %q, want %q", c.kind, c.model, c.surface, got, c.want)
		}
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		p    api.Profile
		want string
	}{
		{api.Profile{PlanLabel: "Ultra plan", PlanName: "Ultra", RateLimitTier: "default_claude_max_20x", SubscriptionStatus: "active"}, "Ultra plan · Max 20x"},
		{api.Profile{PlanLabel: "Max plan", PlanName: "Max", RateLimitTier: "default_claude_max_5x"}, "Max plan · 5x"},
		{api.Profile{PlanName: "Pro", RateLimitTier: "default_claude_pro"}, "Pro plan"},
		{api.Profile{HasClaudeMax: true, SubscriptionStatus: "past_due"}, "Max plan · past due"},
	}
	for _, c := range cases {
		got := BuildPlan(&c.p)
		if got == nil || got.Display != c.want {
			t.Errorf("BuildPlan(%+v) = %+v, want %q", c.p, got, c.want)
		}
	}
	if BuildPlan(&api.Profile{}) != nil {
		t.Error("empty profile should yield nil plan")
	}
	if TierLabel("default_claude_max_20x") != "Max 20x" || TierLabel("") != "" {
		t.Error("TierLabel")
	}
}

func TestSpend(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"spend":{"enabled":true,"used":{"amount_minor":4550,"currency":"USD","exponent":2},"limit":{"amount_minor":5000,"currency":"USD","exponent":2}}}`))
	s := Build(Input{Now: now, Usage: u}).Spend
	if s == nil || s.Used != 45.5 || *s.Limit != 50 || *s.Percent != 91 || s.Severity != SeverityCritical {
		t.Errorf("spend = %+v", s)
	}
	u, _ = api.ParseUsage([]byte(`{"extra_usage":{"is_enabled":true,"monthly_limit":10000,"used_credits":2500,"currency":"usd"}}`))
	s = Build(Input{Now: now, Usage: u}).Spend
	if s == nil || s.Source != "extra_usage" || s.Used != 25 || *s.Limit != 100 || *s.Percent != 25 {
		t.Errorf("extra_usage = %+v", s)
	}
}

func TestLabelsNonASCII(t *testing.T) {
	cases := []struct{ got, want string }{
		{LimitLabel("éclair_kind", "", ""), "Éclair kind"},
		{LimitLabel("seven_day_ébène", "", ""), "Weekly · Ébène"},
		{ShortLabel("seven_day_ébène", "", ""), "Ébène"},
		{TierLabel("default_claude_über_5x"), "Über 5x"},
		{LimitLabel("monthly_moonbeam", "", ""), "Monthly moonbeam"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
