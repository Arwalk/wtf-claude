package api

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// Usage is the tolerant, parsed form of GET /api/oauth/usage.
type Usage struct {
	// Limits is the modern list of rate-limit windows.
	Limits []Limit
	// Legacy holds the older per-window objects (five_hour, seven_day,
	// seven_day_opus, ...) that are non-null, in a stable order.
	Legacy []LegacyWindow
	// ExtraUsage is the pay-as-you-go overflow configuration, if present.
	ExtraUsage *ExtraUsage
	// Spend is the newer money-based view of extra usage, if present.
	Spend *Spend
	// Breakdown splits the 7-day usage by surface (Claude Code, chat, ...).
	Breakdown *Breakdown
}

// Limit is one entry of the "limits" array.
type Limit struct {
	Kind      string
	Group     string
	Percent   *float64
	Severity  string
	ResetsAt  time.Time
	ModelName string
	Surface   string
	IsActive  bool
}

// LegacyWindow is one of the older top-level window objects.
type LegacyWindow struct {
	Key         string
	Utilization float64
	ResetsAt    time.Time
}

// Money is an amount in minor units (e.g. cents) with its exponent.
type Money struct {
	AmountMinor float64
	Currency    string
	Exponent    int
}

// Value returns the amount in major units (e.g. dollars).
func (m Money) Value() float64 {
	return m.AmountMinor / math.Pow10(m.Exponent)
}

// Spend is the "spend" object.
type Spend struct {
	Enabled  bool
	Used     *Money
	Limit    *Money
	Percent  *float64
	Severity string
}

// ExtraUsage is the "extra_usage" object.
type ExtraUsage struct {
	Enabled       bool
	MonthlyLimit  *float64
	UsedCredits   *float64
	Utilization   *float64
	Currency      string
	DecimalPlaces *float64
}

// Breakdown is the "seven_day_breakdown" object.
type Breakdown struct {
	AsOf            time.Time
	WindowStartedAt time.Time
	Rows            []BreakdownRow
}

// BreakdownRow is one surface in the 7-day breakdown.
type BreakdownRow struct {
	Key         string
	DisplayName string
	Percent     float64
}

// ErrNotObject is returned when a response body is not a JSON object.
var ErrNotObject = errors.New("response is not a JSON object")

// legacyOrder lists the known legacy windows in display order. Any other
// "seven_day_*" object carrying a utilization is appended after these.
var legacyOrder = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// ParseUsage parses a usage response. Only a body that is not a JSON object
// is an error; every individual field is optional and tolerant of nulls,
// type surprises and unknown values.
func ParseUsage(b []byte) (*Usage, error) {
	top, ok := decodeObject(b)
	if !ok {
		return nil, ErrNotObject
	}
	u := &Usage{}

	for _, raw := range decodeArray(top["limits"]) {
		if l, ok := parseLimit(raw); ok {
			u.Limits = append(u.Limits, l)
		}
	}

	seen := map[string]bool{}
	addLegacy := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		var w struct {
			Utilization flexFloat `json:"utilization"`
			ResetsAt    flexTime  `json:"resets_at"`
		}
		raw, ok := top[key]
		if !ok || isNull(raw) || json.Unmarshal(raw, &w) != nil || !w.Utilization.OK {
			return
		}
		u.Legacy = append(u.Legacy, LegacyWindow{Key: key, Utilization: w.Utilization.V, ResetsAt: w.ResetsAt.T})
	}
	for _, k := range legacyOrder {
		addLegacy(k)
	}
	var extra []string
	for k := range top {
		if strings.HasPrefix(k, "seven_day_") && k != "seven_day_breakdown" {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		addLegacy(k)
	}

	u.ExtraUsage = parseExtraUsage(top["extra_usage"])
	u.Spend = parseSpend(top["spend"])
	u.Breakdown = parseBreakdown(top["seven_day_breakdown"])
	return u, nil
}

func parseLimit(raw json.RawMessage) (Limit, bool) {
	var r struct {
		Kind     flexString      `json:"kind"`
		Group    flexString      `json:"group"`
		Percent  flexFloat       `json:"percent"`
		Severity flexString      `json:"severity"`
		ResetsAt flexTime        `json:"resets_at"`
		Scope    json.RawMessage `json:"scope"`
		IsActive flexBool        `json:"is_active"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Limit{}, false
	}
	l := Limit{
		Kind:     strings.TrimSpace(string(r.Kind)),
		Group:    strings.TrimSpace(string(r.Group)),
		Percent:  r.Percent.ptr(),
		Severity: strings.ToLower(strings.TrimSpace(string(r.Severity))),
		ResetsAt: r.ResetsAt.T,
		IsActive: bool(r.IsActive),
	}
	if scope, ok := decodeObject(r.Scope); ok {
		l.ModelName = nameOf(scope["model"])
		l.Surface = nameOf(scope["surface"])
	}
	if l.Kind == "" && l.Percent == nil {
		return Limit{}, false
	}
	return l, true
}

func parseMoney(raw json.RawMessage) *Money {
	var m struct {
		AmountMinor flexFloat  `json:"amount_minor"`
		Currency    flexString `json:"currency"`
		Exponent    flexFloat  `json:"exponent"`
	}
	if isNull(raw) || json.Unmarshal(raw, &m) != nil || !m.AmountMinor.OK {
		return nil
	}
	exp := 2
	if m.Exponent.OK && m.Exponent.V >= 0 && m.Exponent.V <= 8 {
		exp = int(m.Exponent.V)
	}
	return &Money{AmountMinor: m.AmountMinor.V, Currency: strings.ToUpper(string(m.Currency)), Exponent: exp}
}

func parseSpend(raw json.RawMessage) *Spend {
	var r struct {
		Used     json.RawMessage `json:"used"`
		Limit    json.RawMessage `json:"limit"`
		Percent  flexFloat       `json:"percent"`
		Severity flexString      `json:"severity"`
		Enabled  flexBool        `json:"enabled"`
	}
	if isNull(raw) || json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return &Spend{
		Enabled:  bool(r.Enabled),
		Used:     parseMoney(r.Used),
		Limit:    parseMoney(r.Limit),
		Percent:  r.Percent.ptr(),
		Severity: strings.ToLower(string(r.Severity)),
	}
}

func parseExtraUsage(raw json.RawMessage) *ExtraUsage {
	var r struct {
		Enabled       flexBool   `json:"is_enabled"`
		MonthlyLimit  flexFloat  `json:"monthly_limit"`
		UsedCredits   flexFloat  `json:"used_credits"`
		Utilization   flexFloat  `json:"utilization"`
		Currency      flexString `json:"currency"`
		DecimalPlaces flexFloat  `json:"decimal_places"`
	}
	if isNull(raw) || json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return &ExtraUsage{
		Enabled:       bool(r.Enabled),
		MonthlyLimit:  r.MonthlyLimit.ptr(),
		UsedCredits:   r.UsedCredits.ptr(),
		Utilization:   r.Utilization.ptr(),
		Currency:      strings.ToUpper(string(r.Currency)),
		DecimalPlaces: r.DecimalPlaces.ptr(),
	}
}

func parseBreakdown(raw json.RawMessage) *Breakdown {
	var r struct {
		AsOf            flexTime        `json:"as_of"`
		WindowStartedAt flexTime        `json:"window_started_at"`
		Rows            json.RawMessage `json:"rows"`
	}
	if isNull(raw) || json.Unmarshal(raw, &r) != nil {
		return nil
	}
	b := &Breakdown{AsOf: r.AsOf.T, WindowStartedAt: r.WindowStartedAt.T}
	for _, rr := range decodeArray(r.Rows) {
		var row struct {
			Key         flexString `json:"key"`
			DisplayName flexString `json:"display_name"`
			Percent     flexFloat  `json:"percent"`
		}
		if json.Unmarshal(rr, &row) != nil || !row.Percent.OK {
			continue
		}
		b.Rows = append(b.Rows, BreakdownRow{
			Key:         string(row.Key),
			DisplayName: strings.TrimSpace(string(row.DisplayName)),
			Percent:     row.Percent.V,
		})
	}
	return b
}
