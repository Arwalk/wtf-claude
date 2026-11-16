// Package report turns raw API responses and local statistics into a
// normalized, render-ready (and JSON-serialisable) Report.
package report

import (
	"math"
	"sort"
	"strings"
	"time"

	"claude-usage/internal/api"
	"claude-usage/internal/localstats"
)

// Severity levels used for colouring.
const (
	SeverityOK       = "ok"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
	// SeverityUnknown is used when a limit's percentage is not known.
	SeverityUnknown = "unknown"
)

// Pace statuses.
const (
	PaceUnder   = "under"
	PaceOnTrack = "on_track"
	PaceOver    = "over"
)

// Report is the normalized output. Its JSON form is the --format json
// contract documented in README.md.
type Report struct {
	Version     string            `json:"version"`
	GeneratedAt time.Time         `json:"generated_at"`
	Plan        *Plan             `json:"plan,omitempty"`
	Limits      []Limit           `json:"limits,omitempty"`
	Breakdown   *Breakdown        `json:"breakdown,omitempty"`
	Spend       *Spend            `json:"spend,omitempty"`
	Local       *localstats.Stats `json:"local,omitempty"`
	Status      Status            `json:"status"`
}

// Status describes data freshness and errors.
type Status struct {
	UsageFetchedAt *time.Time `json:"usage_fetched_at,omitempty"`
	// UsageFromCache is true when usage data came from the on-disk cache.
	UsageFromCache bool `json:"usage_from_cache"`
	// Stale is true when a refresh failed and older cached data is shown.
	Stale        bool   `json:"stale"`
	UsageError   string `json:"usage_error,omitempty"`
	ProfileError string `json:"profile_error,omitempty"`
	LocalError   string `json:"local_error,omitempty"`
	// CredentialSource is "flag", "env", "keychain" or the credentials path.
	CredentialSource string `json:"credential_source,omitempty"`
	// NoLimitData is true when usage was fetched but contained no limits.
	NoLimitData bool `json:"no_limit_data,omitempty"`
}

// Plan is the subscription plan.
type Plan struct {
	Label              string `json:"label,omitempty"`
	Name               string `json:"name,omitempty"`
	Tier               string `json:"tier,omitempty"`
	RateLimitTier      string `json:"rate_limit_tier,omitempty"`
	SubscriptionStatus string `json:"subscription_status,omitempty"`
	ExtraUsageEnabled  bool   `json:"extra_usage_enabled"`
	// Display is the one-line summary, e.g. "Max plan · 20x".
	Display string `json:"display"`
}

// Limit is one rate-limit window.
type Limit struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// ShortLabel tells limits apart in narrow layouts: "All", "Fable", "5h".
	ShortLabel string `json:"short_label"`
	Group      string `json:"group,omitempty"`
	// Source is "limits" or "legacy" (top-level five_hour/seven_day objects).
	Source string `json:"source"`
	// PercentUsed and PercentLeft are nil when unknown: the API sent null or
	// an unreadable value, or the window has reset since the data was
	// fetched (see Expired).
	PercentUsed *float64 `json:"percent_used,omitempty"`
	PercentLeft *float64 `json:"percent_left,omitempty"`
	// Severity is ok, warning, critical or unknown.
	Severity    string `json:"severity"`
	APISeverity string `json:"api_severity,omitempty"`
	Active      bool   `json:"active"`
	// Expired is true when resets_at is in the past: the data predates the
	// reset (typically a stale cached response), so current usage is
	// unknown until the next successful refresh.
	Expired  bool       `json:"expired,omitempty"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// ResetsInSeconds is never negative.
	ResetsInSeconds *int64     `json:"resets_in_seconds,omitempty"`
	WindowSeconds   int64      `json:"window_seconds,omitempty"`
	WindowStart     *time.Time `json:"window_start,omitempty"`
	Pace            *Pace      `json:"pace,omitempty"`
}

// Used returns the used percentage and whether it is known.
func (l Limit) Used() (float64, bool) {
	if l.PercentUsed == nil {
		return 0, false
	}
	return *l.PercentUsed, true
}

// DisplayLeft is the whole-number "% left" shown for a used percentage:
// rounded down, so 99.6% used is 0 (shown as "<1%") and 69.5% used is 30.
// Severity thresholds use the same value so text and colour always agree.
func DisplayLeft(used float64) float64 {
	if used >= 100 {
		return 0
	}
	return math.Min(100, math.Floor(100-used+1e-9))
}

// Pace compares usage with an even burn rate across the window.
type Pace struct {
	// ElapsedFraction of the window, 0..1.
	ElapsedFraction float64 `json:"elapsed_fraction"`
	// ExpectedPercent is what an even pace would have used by now.
	ExpectedPercent float64 `json:"expected_percent"`
	// DeltaPercent = PercentUsed - ExpectedPercent (positive = burning fast).
	DeltaPercent float64 `json:"delta_percent"`
	Status       string  `json:"status"`
	// ProjectedPercent at reset if the current average rate continues.
	ProjectedPercent *float64 `json:"projected_percent,omitempty"`
	// RunsOutAt is when 100% would be reached before reset at the current
	// rate (nil if it will not).
	RunsOutAt *time.Time `json:"runs_out_at,omitempty"`
}

// Breakdown is the 7-day usage split by surface.
type Breakdown struct {
	WindowStartedAt *time.Time     `json:"window_started_at,omitempty"`
	AsOf            *time.Time     `json:"as_of,omitempty"`
	Rows            []BreakdownRow `json:"rows"`
}

// BreakdownRow is one surface.
type BreakdownRow struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	Percent float64 `json:"percent"`
}

// Spend is extra (pay-as-you-go) usage.
type Spend struct {
	Source   string   `json:"source"` // "spend" or "extra_usage"
	Currency string   `json:"currency"`
	Used     float64  `json:"used"`
	Limit    *float64 `json:"limit,omitempty"`
	Percent  *float64 `json:"percent,omitempty"`
	Severity string   `json:"severity"`
}

// Input bundles everything Build needs.
type Input struct {
	Version string
	Now     time.Time
	Usage   *api.Usage
	Profile *api.Profile
	Local   *localstats.Stats
	Status  Status
}

// Build assembles a Report.
func Build(in Input) *Report {
	r := &Report{Version: in.Version, GeneratedAt: in.Now, Status: in.Status, Local: in.Local}
	if in.Profile != nil {
		r.Plan = BuildPlan(in.Profile)
	}
	if in.Usage != nil {
		r.Limits = BuildLimits(in.Usage, in.Now)
		r.Breakdown = buildBreakdown(in.Usage.Breakdown)
		r.Spend = buildSpend(in.Usage)
		r.Status.NoLimitData = len(r.Limits) == 0
	}
	return r
}

// WeekStart returns the start of the current weekly window if the usage
// response reveals it (zero otherwise). A window that has already ended (a
// stale cached response, or the API lagging just after a reset) is ignored
// so callers fall back to a rolling 7 days; the returned start is therefore
// always within the last 7 days.
func WeekStart(u *api.Usage, now time.Time) time.Time {
	if u == nil {
		return time.Time{}
	}
	if b := u.Breakdown; b != nil && !b.WindowStartedAt.IsZero() {
		ws := b.WindowStartedAt
		if !ws.After(now) && ws.Add(week).After(now) {
			return ws
		}
	}
	for _, l := range u.Limits {
		if windowFor(l.Kind, l.Group) == week && !l.ResetsAt.IsZero() && l.ResetsAt.After(now) {
			if ws := l.ResetsAt.Add(-week); !ws.After(now) {
				return ws
			}
		}
	}
	return time.Time{}
}

const (
	week    = 7 * 24 * time.Hour
	session = 5 * time.Hour
)

// windowFor infers the window length of a limit kind/group.
func windowFor(kind, group string) time.Duration {
	k := strings.ToLower(kind + " " + group)
	switch {
	case strings.Contains(k, "five_hour"), strings.Contains(k, "5h"), strings.Contains(k, "session"):
		return session
	case strings.Contains(k, "week"), strings.Contains(k, "seven_day"), strings.Contains(k, "7d"):
		return week
	case strings.Contains(k, "daily"), strings.Contains(k, "day"):
		return 24 * time.Hour
	}
	return 0
}

// LimitLabel returns the display label for a limit.
func LimitLabel(kind, model, surface string) string {
	scope := model
	if scope == "" {
		scope = humanize(surface)
	}
	switch k := strings.ToLower(kind); {
	case k == "weekly_all":
		return "Weekly · all models"
	case k == "weekly_scoped":
		if scope == "" {
			return "Weekly · scoped"
		}
		return "Weekly · " + scope
	case k == "five_hour", k == "session", strings.HasPrefix(k, "session"), strings.HasPrefix(k, "five_hour"):
		if scope != "" && k != "five_hour" && k != "session" {
			return "Session · " + scope
		}
		return "Session (5h)"
	case k == "seven_day":
		return "Weekly · all models"
	case strings.HasPrefix(k, "seven_day_"):
		return "Weekly · " + titleWord(strings.TrimPrefix(k, "seven_day_"))
	}
	label := humanize(kind)
	if label == "" {
		label = "Limit"
	}
	if scope != "" {
		label += " · " + scope
	}
	return label
}

// ShortLabel returns a terse label that still tells limits apart when the
// full label does not fit: "All" for all models, the model or surface for
// scoped limits, "5h" for the session window. Unknown kinds keep their full
// label.
func ShortLabel(kind, model, surface string) string {
	scope := model
	if scope == "" {
		scope = humanize(surface)
	}
	switch k := strings.ToLower(kind); {
	case k == "weekly_all", k == "seven_day":
		return "All"
	case k == "weekly_scoped":
		if scope == "" {
			return "Scoped"
		}
		return scope
	case k == "five_hour", k == "session", strings.HasPrefix(k, "session"), strings.HasPrefix(k, "five_hour"):
		if scope != "" && k != "five_hour" && k != "session" {
			return "5h " + scope
		}
		return "5h"
	case strings.HasPrefix(k, "seven_day_"):
		return titleWord(strings.TrimPrefix(k, "seven_day_"))
	}
	return LimitLabel(kind, model, surface)
}

// humanize turns "weekly_opus_thing" into "Weekly opus thing".
func humanize(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(s))
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func titleWord(s string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(s))
	for i, w := range words {
		if w == "oauth" {
			words[i] = "OAuth"
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// severityFor combines the thresholds (green above 30% left, yellow down to
// 10% left, red below) with the API's own label. The thresholds apply to the
// displayed "% left" (DisplayLeft), i.e. green <70% used, yellow <90%, red
// >=90% for whole percentages, without rounding ever making the colour
// contradict the number shown.
func severityFor(used float64, apiSeverity string) string {
	left := DisplayLeft(used)
	sev := SeverityOK
	switch {
	case left <= 10:
		sev = SeverityCritical
	case left <= 30:
		sev = SeverityWarning
	}
	return withAPISeverity(sev, apiSeverity)
}

func withAPISeverity(sev, apiSeverity string) string {
	switch strings.ToLower(apiSeverity) {
	case "critical", "error", "exceeded", "blocked", "limited", "danger", "severe", "high":
		sev = SeverityCritical
	case "warning", "warn", "elevated", "medium", "approaching":
		if sev == SeverityOK || sev == SeverityUnknown {
			sev = SeverityWarning
		}
	}
	return sev
}

// spendSeverity applies the thresholds to a used percentage shown rounded
// to the nearest whole number (as the spend line shows it).
func spendSeverity(used float64, apiSeverity string) string {
	shown := math.Floor(used + 0.5)
	sev := SeverityOK
	switch {
	case shown >= 90:
		sev = SeverityCritical
	case shown >= 70:
		sev = SeverityWarning
	}
	return withAPISeverity(sev, apiSeverity)
}

// BuildLimits normalizes the limits array, falling back to legacy windows.
func BuildLimits(u *api.Usage, now time.Time) []Limit {
	var out []Limit
	weekStart := time.Time{}
	if u.Breakdown != nil {
		weekStart = u.Breakdown.WindowStartedAt
	}
	for _, l := range u.Limits {
		lim := makeLimit(l.Kind, l.Group, "limits", l.Percent, l.Severity, l.IsActive, l.ResetsAt, weekStart, now)
		lim.Label = LimitLabel(l.Kind, l.ModelName, l.Surface)
		lim.ShortLabel = ShortLabel(l.Kind, l.ModelName, l.Surface)
		out = append(out, lim)
	}
	if len(out) == 0 {
		for _, w := range u.Legacy {
			pct := w.Utilization
			lim := makeLimit(w.Key, "", "legacy", &pct, "", false, w.ResetsAt, weekStart, now)
			lim.Label = LimitLabel(w.Key, "", "")
			lim.ShortLabel = ShortLabel(w.Key, "", "")
			out = append(out, lim)
		}
		// Mark the most-used legacy window as the binding one.
		best, bestPct := -1, 0.0
		for i := range out {
			if p, ok := out[i].Used(); ok && p > bestPct {
				best, bestPct = i, p
			}
		}
		if best >= 0 {
			out[best].Active = true
		}
	}
	return out
}

func makeLimit(kind, group, source string, pctp *float64, apiSev string, active bool, resets, weekStart, now time.Time) Limit {
	l := Limit{
		Kind:        kind,
		Group:       group,
		Source:      source,
		APISeverity: apiSev,
		Active:      active,
		Severity:    SeverityUnknown,
	}
	if !resets.IsZero() {
		r := resets
		l.ResetsAt = &r
		secs := max(0, int64(math.Round(resets.Sub(now).Seconds())))
		l.ResetsInSeconds = &secs
		// The window has reset since this data was fetched: whatever it
		// said about usage no longer applies.
		l.Expired = !resets.After(now)
	}
	if pctp != nil && !math.IsNaN(*pctp) && !math.IsInf(*pctp, 0) && !l.Expired {
		pct := math.Max(0, *pctp)
		left := math.Max(0, 100-pct)
		l.PercentUsed, l.PercentLeft = &pct, &left
		l.Severity = severityFor(pct, apiSev)
	} else if !l.Expired {
		l.Severity = withAPISeverity(SeverityUnknown, apiSev)
	}
	win := windowFor(kind, group)
	if win == 0 || resets.IsZero() {
		return l
	}
	l.WindowSeconds = int64(win.Seconds())
	start := resets.Add(-win)
	// Prefer the server-reported start of the weekly window when it agrees
	// with this limit (same reset time, give or take an hour).
	if win == week && !weekStart.IsZero() {
		if d := resets.Sub(weekStart); d > 0 && math.Abs((d-week).Hours()) <= 1 {
			start = weekStart
		}
	}
	l.WindowStart = &start
	if pct, ok := l.Used(); ok {
		l.Pace = computePace(pct, start, resets, now)
	}
	return l
}

// computePace compares used percent against an even burn rate.
func computePace(used float64, start, end, now time.Time) *Pace {
	total := end.Sub(start)
	if total <= 0 || now.Before(start) || !now.Before(end) {
		return nil
	}
	elapsed := now.Sub(start)
	frac := elapsed.Seconds() / total.Seconds()
	p := &Pace{
		ElapsedFraction: round(frac, 4),
		ExpectedPercent: round(frac*100, 1),
	}
	p.DeltaPercent = round(used-p.ExpectedPercent, 1)
	switch {
	case used >= 100:
		p.Status = PaceOver
	case math.Abs(p.DeltaPercent) <= 5:
		p.Status = PaceOnTrack
	case p.DeltaPercent > 0:
		p.Status = PaceOver
	default:
		p.Status = PaceUnder
	}
	// Projections are noise very early in a window.
	if frac < 0.03 || used <= 0 {
		return p
	}
	proj := round(used/frac, 1)
	p.ProjectedPercent = &proj
	if used >= 100 {
		t := now
		p.RunsOutAt = &t
	} else if proj > 100 {
		t := start.Add(time.Duration(float64(elapsed) * 100 / used)).Truncate(time.Minute)
		if t.Before(end) {
			p.RunsOutAt = &t
		}
	}
	return p
}

func round(v float64, places int) float64 {
	f := math.Pow10(places)
	return math.Round(v*f) / f
}

// BuildPlan normalizes profile data into a Plan.
func BuildPlan(p *api.Profile) *Plan {
	pl := &Plan{
		Label:              p.PlanLabel,
		Name:               p.PlanName,
		RateLimitTier:      p.RateLimitTier,
		Tier:               TierLabel(p.RateLimitTier),
		SubscriptionStatus: p.SubscriptionStatus,
		ExtraUsageEnabled:  p.ExtraUsageEnabled,
	}
	label := pl.Label
	if label == "" && pl.Name != "" {
		label = pl.Name + " plan"
	}
	if label == "" {
		switch {
		case p.HasClaudeMax:
			label = "Max plan"
		case p.HasClaudePro:
			label = "Pro plan"
		case p.OrganizationType != "":
			label = humanize(strings.TrimPrefix(p.OrganizationType, "claude_")) + " plan"
		}
	}
	tier := pl.Tier
	// Avoid "Max plan · Max 20x": drop the repeated plan name.
	if pl.Name != "" && strings.HasPrefix(strings.ToLower(tier), strings.ToLower(pl.Name)+" ") {
		tier = strings.TrimSpace(tier[len(pl.Name):])
	}
	if strings.EqualFold(tier, pl.Name) || strings.EqualFold(tier+" plan", label) {
		tier = ""
	}
	parts := []string{}
	for _, s := range []string{label, tier} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if st := strings.ToLower(pl.SubscriptionStatus); st != "" && st != "active" {
		parts = append(parts, strings.ReplaceAll(st, "_", " "))
	}
	pl.Display = strings.Join(parts, " · ")
	if pl.Display == "" {
		return nil
	}
	return pl
}

// TierLabel maps a rate_limit_tier like "default_claude_max_20x" to
// "Max 20x".
func TierLabel(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return ""
	}
	t = strings.TrimPrefix(t, "default_")
	t = strings.TrimPrefix(t, "claude_")
	words := strings.Fields(strings.ReplaceAll(t, "_", " "))
	for i, w := range words {
		if len(w) > 1 && strings.HasSuffix(w, "x") && strings.Trim(w[:len(w)-1], "0123456789") == "" {
			continue // "20x"
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func buildBreakdown(b *api.Breakdown) *Breakdown {
	if b == nil {
		return nil
	}
	out := &Breakdown{}
	if !b.WindowStartedAt.IsZero() {
		t := b.WindowStartedAt
		out.WindowStartedAt = &t
	}
	if !b.AsOf.IsZero() {
		t := b.AsOf
		out.AsOf = &t
	}
	for _, r := range b.Rows {
		if r.Percent <= 0 {
			continue
		}
		name := r.DisplayName
		if name == "" {
			name = humanize(r.Key)
		}
		out.Rows = append(out.Rows, BreakdownRow{Key: r.Key, Name: name, Percent: r.Percent})
	}
	if len(out.Rows) == 0 {
		return nil
	}
	sort.SliceStable(out.Rows, func(i, j int) bool { return out.Rows[i].Percent > out.Rows[j].Percent })
	return out
}

func buildSpend(u *api.Usage) *Spend {
	if s := u.Spend; s != nil && s.Enabled && s.Used != nil {
		sp := &Spend{Source: "spend", Currency: s.Used.Currency, Used: s.Used.Value(), Percent: s.Percent}
		if s.Limit != nil {
			v := s.Limit.Value()
			sp.Limit = &v
			if sp.Currency == "" {
				sp.Currency = s.Limit.Currency
			}
		}
		if sp.Percent == nil && sp.Limit != nil && *sp.Limit > 0 {
			p := round(sp.Used / *sp.Limit * 100, 1)
			sp.Percent = &p
		}
		pct := 0.0
		if sp.Percent != nil {
			pct = *sp.Percent
		}
		sp.Severity = spendSeverity(pct, s.Severity)
		return sp
	}
	if e := u.ExtraUsage; e != nil && e.Enabled {
		// extra_usage amounts are in minor units (cents) unless
		// decimal_places says otherwise.
		div := 100.0
		if e.DecimalPlaces != nil && *e.DecimalPlaces >= 0 && *e.DecimalPlaces <= 8 {
			div = math.Pow10(int(*e.DecimalPlaces))
		}
		sp := &Spend{Source: "extra_usage", Currency: e.Currency, Percent: e.Utilization}
		if e.UsedCredits != nil {
			sp.Used = *e.UsedCredits / div
		}
		if e.MonthlyLimit != nil {
			v := *e.MonthlyLimit / div
			sp.Limit = &v
		}
		if sp.Percent == nil && sp.Limit != nil && *sp.Limit > 0 {
			p := round(sp.Used / *sp.Limit * 100, 1)
			sp.Percent = &p
		}
		pct := 0.0
		if sp.Percent != nil {
			pct = *sp.Percent
		}
		sp.Severity = spendSeverity(pct, "")
		return sp
	}
	return nil
}
