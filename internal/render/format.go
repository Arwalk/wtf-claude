package render

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Duration formats d with at most two units: "5d 17h", "13h 50m", "42m".
func Duration(d time.Duration) string {
	return duration(d, " ")
}

// CompactDuration is Duration without the space: "5d17h".
func CompactDuration(d time.Duration) string {
	return duration(d, "")
}

func duration(d time.Duration, sep string) string {
	if d < time.Minute {
		if d <= 0 {
			return "now"
		}
		return "<1m"
	}
	mins := int64(d.Round(time.Minute) / time.Minute)
	days, hours, m := mins/(24*60), (mins/60)%24, mins%60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%s%dh", days, sep, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && m > 0:
		return fmt.Sprintf("%dh%s%dm", hours, sep, m)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", m)
}

// WallClock formats t relative to now in loc: "15:04" for today,
// "Mon 15:04" within the next 6 days, "Jan 2 15:04" otherwise.
func WallClock(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("15:04")
	}
	if d := t.Sub(now); d > -6*24*time.Hour && d < 6*24*time.Hour+12*time.Hour {
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}

// Number humanizes counts: 999, 1.2k, 34k, 1.2M, 12M, 3.4B.
func Number(n int64) string {
	if n < 0 {
		return "-" + Number(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	units := []string{"k", "M", "B", "T"}
	v := float64(n)
	for i, u := range units {
		v /= 1000
		// Promote 999.6k to 1M rather than printing "1000k".
		if v >= 999.5 && i < len(units)-1 {
			continue
		}
		if v < 9.95 {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", math.Round(v*10)/10), ".0") + u
		}
		return fmt.Sprintf("%.0f%s", math.Round(v), u)
	}
	return fmt.Sprintf("%d", n)
}

// Percent formats a percentage without decimals unless < 10 and fractional.
func Percent(p float64) string {
	if p > 0 && p < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", math.Floor(p+0.5))
}

var currencySymbols = map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CAD": "CA$", "AUD": "A$"}

// Money formats an amount in major units.
func Money(v float64, currency string) string {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if sym, ok := currencySymbols[cur]; ok {
		if cur == "JPY" {
			return fmt.Sprintf("%s%.0f", sym, v)
		}
		return fmt.Sprintf("%s%.2f", sym, v)
	}
	if cur == "" {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, cur)
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// ModelName shortens "claude-opus-4-1-20250805" to "opus-4-1".
func ModelName(m string) string {
	m = strings.TrimPrefix(strings.TrimSpace(m), "claude-")
	m = dateSuffix.ReplaceAllString(m, "")
	if m == "" {
		return "unknown"
	}
	return m
}
