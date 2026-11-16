package render

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-usage/internal/api"
	"claude-usage/internal/localstats"
	"claude-usage/internal/report"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Fixed clock: the fixture's as_of, rendered in a fixed UTC+1 zone.
var (
	fixedNow = time.Date(2026, 11, 16, 16, 47, 48, 0, time.UTC)
	cet      = time.FixedZone("CET", 3600)
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sampleLocal() *localstats.Stats {
	mk := func(label string, total, out int64, msgs, sess int, models ...localstats.ModelUsage) localstats.Period {
		return localstats.Period{Label: label, Tokens: localstats.Tokens{Total: total, Output: out}, Messages: msgs, Sessions: sess, Models: models}
	}
	return &localstats.Stats{
		Today: mk("today", 48_300_000, 212_000, 640, 7),
		Week: mk("week", 311_000_000, 1_450_000, 4120, 38,
			localstats.ModelUsage{Model: "claude-opus-5-5", Share: 0.93},
			localstats.ModelUsage{Model: "claude-sonnet-4-5-20250929", Share: 0.06},
			localstats.ModelUsage{Model: "claude-haiku-4-5", Share: 0.01}),
		WeekFromAPI: true,
	}
}

func fixtureReport(t *testing.T) *report.Report {
	t.Helper()
	u, err := api.ParseUsage(mustRead(t, "../../testdata/usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := api.ParseProfile(mustRead(t, "../../testdata/profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	fetched := fixedNow.Add(-40 * time.Second)
	return report.Build(report.Input{
		Version: "test", Now: fixedNow, Usage: u, Profile: p, Local: sampleLocal(),
		Status: report.Status{UsageFetchedAt: &fetched},
	})
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file (run go test ./internal/render -update): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func renderString(t *testing.T, r *report.Report, opt Options) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Text(&buf, r, opt); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestGoldenWidths(t *testing.T) {
	r := fixtureReport(t)
	for _, w := range []int{80, 60, 38, 28, 20, 12} {
		for _, compact := range []bool{false, true} {
			name := fmt.Sprintf("full_w%d", w)
			if compact {
				name = fmt.Sprintf("compact_w%d", w)
			}
			t.Run(name, func(t *testing.T) {
				got := renderString(t, r, Options{Width: w, Compact: compact, Loc: cet})
				golden(t, name, got)
				for i, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
					if cw := cellWidth(l); cw > w {
						t.Errorf("line %d is %d cells wide, limit %d: %q", i+1, cw, w, l)
					}
				}
			})
		}
	}
}

func TestGoldenANSI(t *testing.T) {
	got := renderString(t, fixtureReport(t), Options{Width: 60, Color: true, Loc: cet})
	golden(t, "ansi_w60", got)
	if !strings.Contains(got, "\x1b[") {
		t.Fatal("expected ANSI escapes")
	}
	if strings.Contains(got, "[red]") || strings.Contains(got, "[green]") {
		t.Fatal("must not emit tview colour tags")
	}
}

func TestPlainHasNoEscapes(t *testing.T) {
	got := renderString(t, fixtureReport(t), Options{Width: 60, Loc: cet})
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain output contains escapes: %q", got)
	}
}

func TestTinyWidthNeverOverflows(t *testing.T) {
	r := fixtureReport(t)
	for w := 1; w <= 45; w++ {
		for _, compact := range []bool{false, true} {
			got := renderString(t, r, Options{Width: w, Compact: compact, Loc: cet})
			lim := max(w, MinWidth)
			for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if cellWidth(l) > lim {
					t.Fatalf("width %d compact=%v: %q is %d cells", w, compact, l, cellWidth(l))
				}
			}
		}
	}
}

func TestContentAt60(t *testing.T) {
	got := renderString(t, fixtureReport(t), Options{Width: 60, Loc: cet})
	for _, want := range []string{
		"Ultra plan · Max 20x",
		"Weekly · all models",
		"55% left",
		"▸Weekly · Fable",
		"31% left",
		"resets in 5d 17h (Sun 11:00)",
		"over pace",
		"runs out in",
		"Claude Code 99% · Cowork 1%",
		"Today",
		"48M tok",
		"opus-5-5 93%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "user@example.com") || strings.Contains(got, "Example User") {
		t.Error("personal details must not be rendered")
	}
	if strings.Contains(got, "Extra usage") {
		t.Error("extra usage is disabled in the fixture and must be hidden")
	}
}

func TestStaleAndErrorNotes(t *testing.T) {
	r := fixtureReport(t)
	fetched := fixedNow.Add(-12 * time.Minute)
	r.Status = report.Status{Stale: true, UsageFetchedAt: &fetched, UsageError: "rate limited (429)"}
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "stale · fetched 12m ago · rate limited (429)") {
		t.Errorf("missing stale note:\n%s", got)
	}

	empty := report.Build(report.Input{Now: fixedNow, Local: sampleLocal(),
		Status: report.Status{UsageError: "network error"}})
	got = renderString(t, empty, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "usage unavailable: network error") || !strings.Contains(got, "Today") {
		t.Errorf("want error line plus local stats:\n%s", got)
	}
	if n := strings.Count(got, "network error"); n != 1 {
		t.Errorf("error should be a single line, got %d", n)
	}
}

func TestNoLimits(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"limits":[],"five_hour":null}`))
	r := report.Build(report.Input{Now: fixedNow, Usage: u})
	got := renderString(t, r, Options{Width: 40, Loc: cet})
	if !strings.Contains(got, "no usage limits reported") {
		t.Errorf("got %q", got)
	}
}

func TestSpendShownWhenEnabled(t *testing.T) {
	u, _ := api.ParseUsage([]byte(`{"spend":{"enabled":true,"used":{"amount_minor":1234,"currency":"USD","exponent":2},
		"limit":{"amount_minor":5000,"currency":"USD","exponent":2},"percent":24.68}}`))
	r := report.Build(report.Input{Now: fixedNow, Usage: u})
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "Extra usage: $12.34 / $50.00 · 25%") {
		t.Errorf("got %q", got)
	}
	got = renderString(t, r, Options{Width: 20, Loc: cet})
	if !strings.Contains(got, "Extra: $12.34/$50") {
		t.Errorf("narrow got %q", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, fixtureReport(t)); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "generated_at", "plan", "limits", "breakdown", "local", "status"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json missing %q", k)
		}
	}
	if strings.Contains(buf.String(), "example.com") {
		t.Error("json must not contain email")
	}
}

func TestFormatters(t *testing.T) {
	cases := map[string]string{
		Number(0): "0", Number(999): "999", Number(1000): "1k", Number(1234): "1.2k",
		Number(34_000): "34k", Number(999_999): "1M", Number(1_250_000): "1.3M", Number(12_400_000): "12M",
		Number(3_400_000_000): "3.4B",
		Duration(5*24*time.Hour + 17*time.Hour + 20*time.Minute): "5d 17h",
		Duration(13*time.Hour + 50*time.Minute):                  "13h 50m",
		Duration(42 * time.Minute):                               "42m",
		Duration(30 * time.Second):                               "<1m",
		Duration(2 * 24 * time.Hour):                             "2d",
		CompactDuration(3*time.Hour + 5*time.Minute):             "3h5m",
		Money(12.5, "USD"):                                       "$12.50",
		Money(3, "CHF"):                                          "3.00 CHF",
		ModelName("claude-sonnet-4-5-20250929"):                  "sonnet-4-5",
		Percent(0.4):                                             "<1%",
		Percent(68.6):                                            "69%",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestWallClock(t *testing.T) {
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cet)
	if got := WallClock(now.Add(time.Hour), now, cet); got != "23:00" {
		t.Errorf("same day: %q", got)
	}
	if got := WallClock(now.Add(3*time.Hour), now, cet); got != "Tue 01:00" {
		t.Errorf("next day: %q", got)
	}
	if got := WallClock(now.Add(9*24*time.Hour), now, cet); got != "Nov 25 22:00" {
		t.Errorf("far: %q", got)
	}
}

func reportFrom(t *testing.T, body string, local *localstats.Stats) *report.Report {
	t.Helper()
	u, err := api.ParseUsage([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return report.Build(report.Input{Now: fixedNow, Usage: u, Local: local})
}

func splitLines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

// A null or unreadable percent renders as unknown, never as "100% left".
func TestUnknownPercentRendering(t *testing.T) {
	for _, p := range []string{`null`, `"n/a"`} {
		r := reportFrom(t, `{"limits":[{"kind":"weekly_all","percent":`+p+`,"resets_at":"2026-11-22T10:00:00.000000+00:00"}]}`, nil)
		got := renderString(t, r, Options{Width: 60, Loc: cet})
		if !strings.Contains(got, "? left") || strings.Contains(got, "100% left") || strings.Contains(got, "pace") || strings.Contains(got, "█") {
			t.Errorf("percent %s:\n%s", p, got)
		}
		if !strings.Contains(got, "resets in 5d 17h") {
			t.Errorf("reset countdown missing:\n%s", got)
		}
		col := renderString(t, r, Options{Width: 60, Color: true, Loc: cet})
		if strings.Contains(col, "\x1b[1;32m") {
			t.Errorf("unknown percent coloured green: %q", col)
		}
	}
}

// After resets_at has passed (stale cache across a reset) the old
// percentage is not shown.
func TestExpiredLimitRendering(t *testing.T) {
	r := reportFrom(t, `{"limits":[
		{"kind":"five_hour","percent":30,"resets_at":"2026-11-16T15:47:48Z","is_active":true},
		{"kind":"weekly_all","percent":95,"severity":"critical","resets_at":"2026-11-15T10:00:00Z"}]}`, nil)
	for _, compact := range []bool{false, true} {
		got := renderString(t, r, Options{Width: 60, Compact: compact, Loc: cet})
		if strings.Contains(got, "70% left") || strings.Contains(got, "5% left") || strings.Contains(got, "↻now") {
			t.Errorf("compact=%v shows pre-reset data:\n%s", compact, got)
		}
		if strings.Count(got, "reset") < 2 {
			t.Errorf("compact=%v: expected reset markers:\n%s", compact, got)
		}
		if !compact && !strings.Contains(got, "reset · refreshing") {
			t.Errorf("missing refresh note:\n%s", got)
		}
	}
}

// "% left" is rounded down so it never claims more than is left, and the
// colour matches the number shown.
func TestLeftTextMatchesColour(t *testing.T) {
	cases := []struct {
		used     float64
		text     string
		colorSGR string
	}{
		{99.6, "<1% left", "1;31"},
		{100, "0% left", "1;31"},
		{89.5, "10% left", "1;31"},
		{69.5, "30% left", "1;33"},
		{69, "31% left", "1;32"},
		{0.4, "99% left", "1;32"},
	}
	for _, c := range cases {
		r := reportFrom(t, fmt.Sprintf(`{"limits":[{"kind":"weekly_all","percent":%v}]}`, c.used), nil)
		got := renderString(t, r, Options{Width: 60, Color: true, Loc: cet})
		if !strings.Contains(got, "\x1b["+c.colorSGR+"m"+c.text) {
			t.Errorf("used %v: want %q in SGR %s, got %q", c.used, c.text, c.colorSGR, got)
		}
	}
}

// In compact mode at small widths the labels must still tell the limits
// apart (short labels instead of an identical cut-off prefix).
func TestCompactLabelsStayDistinct(t *testing.T) {
	r := fixtureReport(t)
	for w := MinWidth; w <= 60; w++ {
		lines := splitLines(renderString(t, r, Options{Width: w, Compact: true, Loc: cet}))
		if len(lines) != 2 {
			t.Fatalf("w=%d: %q", w, lines)
		}
		a, b := labelPart(lines[0]), labelPart(lines[1])
		if a == b || strings.Contains(a, "…") || strings.Contains(b, "…") {
			t.Errorf("w=%d: labels %q / %q", w, a, b)
		}
	}
	// Full layout: the label is never cut at w=36 either.
	got := renderString(t, r, Options{Width: 36, Loc: cet})
	if strings.Contains(got, "mode…") || !strings.Contains(got, "Weekly · all models") {
		t.Errorf("full w=36:\n%s", got)
	}
}

// labelPart is the text before the first bar/percentage on a limit row.
func labelPart(l string) string {
	l = strings.TrimLeft(l, " ▸")
	if i := strings.IndexAny(l, "█░0123456789<?"); i >= 0 {
		l = l[:i]
	}
	return strings.TrimSpace(l)
}

// Stacked bars have the same length whatever the "% left" text width.
func TestStackedBarsEqualLength(t *testing.T) {
	r := reportFrom(t, `{"limits":[
		{"kind":"weekly_all","percent":95},{"kind":"five_hour","percent":30},
		{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Fable"}}}]}`, nil)
	for _, w := range []int{20, 28, 30} {
		got := renderString(t, r, Options{Width: w, Loc: cet})
		var lens []int
		for _, l := range splitLines(got) {
			if strings.ContainsAny(l, "█░") {
				lens = append(lens, strings.Count(l, "█")+strings.Count(l, "░"))
			}
		}
		if len(lens) != 3 || lens[0] != lens[1] || lens[1] != lens[2] {
			t.Errorf("w=%d bar lengths %v:\n%s", w, lens, got)
		}
	}
}

// Narrow local stats keep short labels so Today and Week are told apart.
func TestNarrowLocalLabels(t *testing.T) {
	r := report.Build(report.Input{Now: fixedNow, Local: sampleLocal()})
	for _, w := range []int{12, 16, 19} {
		got := renderString(t, r, Options{Width: w, Loc: cet})
		if !strings.Contains(got, "Day ") || !strings.Contains(got, "Wk  ") {
			t.Errorf("w=%d:\n%s", w, got)
		}
	}
}

func sessionReport(t *testing.T) *report.Report {
	t.Helper()
	var raw map[string]any
	json.Unmarshal(mustRead(t, "../../testdata/usage.json"), &raw)
	limits := raw["limits"].([]any)
	session := map[string]any{"kind": "five_hour", "group": "session", "percent": 91, "resets_at": "2026-11-16T18:00:00Z"}
	raw["limits"] = append([]any{session}, limits...)
	raw["spend"] = map[string]any{"enabled": true, "used": map[string]any{"amount_minor": 1234, "currency": "USD", "exponent": 2},
		"limit": map[string]any{"amount_minor": 5000, "currency": "USD", "exponent": 2}}
	b, _ := json.Marshal(raw)
	u, err := api.ParseUsage(b)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := api.ParseProfile(mustRead(t, "../../testdata/profile.json"))
	fetched := fixedNow.Add(-20 * time.Minute)
	return report.Build(report.Input{Now: fixedNow, Usage: u, Profile: p, Local: sampleLocal(),
		Status: report.Status{UsageFetchedAt: &fetched, Stale: true, UsageError: "rate limited (429)"}})
}

// With a height (WTF_WIDGET_HEIGHT) the output never exceeds it, and the
// binding limit and stale note are kept.
func TestHeightFitting(t *testing.T) {
	r := sessionReport(t)
	tall := splitLines(renderString(t, r, Options{Width: 60, Loc: cet}))
	if len(tall) <= 12 {
		t.Fatalf("fixture should need more than 12 rows, got %d", len(tall))
	}
	for h := 1; h <= len(tall)+1; h++ {
		for _, compact := range []bool{false, true} {
			got := splitLines(renderString(t, r, Options{Width: 60, Height: h, Compact: compact, Loc: cet}))
			if len(got) > h {
				t.Errorf("h=%d compact=%v: %d lines:\n%s", h, compact, len(got), strings.Join(got, "\n"))
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, "▸") {
				t.Errorf("h=%d compact=%v: active limit hidden:\n%s", h, compact, joined)
			}
			if h >= 2 && !strings.Contains(joined, "stale") {
				t.Errorf("h=%d compact=%v: stale note hidden:\n%s", h, compact, joined)
			}
		}
	}
	// Enough room: the full layout keeps the local stats and limits.
	got := renderString(t, r, Options{Width: 60, Height: 12, Loc: cet})
	for _, want := range []string{"Session (5h)", "Weekly · all models", "Weekly · Fable", "Today", "stale"} {
		if !strings.Contains(got, want) {
			t.Errorf("h=12 missing %q:\n%s", want, got)
		}
	}
	// Compact with 2 rows and 3 limits + stale: the binding limit wins.
	got = renderString(t, r, Options{Width: 60, Height: 2, Compact: true, Loc: cet})
	if !strings.Contains(got, "▸Weekly · Fable") || !strings.Contains(got, "stale") {
		t.Errorf("compact h=2:\n%s", got)
	}
	// Compact with room for "+N more" (4 limits, no stale note).
	r.Status = report.Status{}
	extra := r.Limits[1]
	extra.Label, extra.ShortLabel, extra.Active = "Weekly · Opus", "Opus", false
	r.Limits = append(r.Limits, extra)
	got = renderString(t, r, Options{Width: 60, Height: 3, Compact: true, Loc: cet})
	if !strings.Contains(got, "+2 more") || !strings.Contains(got, "▸Weekly · Fable") || !strings.Contains(got, "Session") {
		t.Errorf("compact h=3:\n%s", got)
	}
}

func TestLocalErrorShown(t *testing.T) {
	r := report.Build(report.Input{Now: fixedNow, Local: &localstats.Stats{},
		Status: report.Status{LocalError: "permission denied (/home/u/.claude/projects/p)"}})
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "local stats unavailable: permission denied") || strings.Contains(got, "no activity") {
		t.Errorf("want the error instead of 'no activity':\n%s", got)
	}
	// With data the stats are shown as usual (the error stays JSON-only).
	r = report.Build(report.Input{Now: fixedNow, Local: sampleLocal(),
		Status: report.Status{LocalError: "permission denied (/x)"}})
	got = renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "Today") || strings.Contains(got, "unavailable") {
		t.Errorf("partial error must not hide stats:\n%s", got)
	}
}

func TestStripControls(t *testing.T) {
	cases := map[string]string{
		"plain":                "plain",
		"Weekly · Fable ▸ 日本":  "Weekly · Fable ▸ 日本",
		"a\x1b[31mb":           "a[31mb",
		"t\x1b]0;x\x07":        "t]0;x",
		"line\nbreak\ttab":     "linebreaktab",
		"c1\u009bx":            "c1x",
		"bidi\u202eevil\u2066": "bidievil",
	}
	for in, want := range cases {
		if got := stripControls(in); got != want {
			t.Errorf("stripControls(%q) = %q, want %q", in, got, want)
		}
	}
}

// Text from the API and transcripts can never inject escapes or extra rows.
func TestUntrustedTextIsSanitized(t *testing.T) {
	r := fixtureReport(t)
	r.Plan.Display = "Ultra\nplan"
	r.Limits[0].Label = "Weekly \x1b[2J\x1b]0;pwned\x07 all"
	r.Limits[0].ShortLabel = "All\x1b[31m"
	r.Breakdown.Rows[0].Name = "Claude\u202eCode"
	r.Local.Week.Models[0].Model = "claude-opus\x1b[5m"
	clean := renderString(t, fixtureReport(t), Options{Width: 60, Loc: cet})
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Errorf("control characters leaked:\n%q", got)
	}
	if strings.Count(got, "\n") != strings.Count(clean, "\n") {
		t.Errorf("a newline in a label added a row:\n%s", got)
	}
	for i, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if cw := cellWidth(l); cw > 60 {
			t.Errorf("line %d is %d cells: %q", i+1, cw, l)
		}
	}
}
