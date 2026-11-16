package api

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// The usage endpoint is undocumented and its shape changes over time. These
// "flex" scalar types accept whatever reasonable JSON shows up (null, numbers
// encoded as strings, ...) and never fail decoding, so one odd field cannot
// take down the whole response.

var jsonNull = []byte("null")

func isNull(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) == 0 || bytes.Equal(b, jsonNull)
}

// flexFloat accepts a JSON number, a numeric string (optionally with a
// trailing "%"), or null.
type flexFloat struct {
	V  float64
	OK bool
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	*f = flexFloat{}
	if isNull(b) {
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexFloat{V: n, OK: true}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
		if n, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			*f = flexFloat{V: n, OK: true}
		}
	}
	return nil
}

func (f flexFloat) ptr() *float64 {
	if !f.OK {
		return nil
	}
	v := f.V
	return &v
}

// flexString accepts a string, a number or a bool (stringified), or null.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	*s = ""
	if isNull(b) {
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = flexString(str)
		return nil
	}
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] != '{' && b[0] != '[' {
		*s = flexString(b)
	}
	return nil
}

// flexBool accepts true/false, "true"/"false", 0/1 or null.
type flexBool bool

func (v *flexBool) UnmarshalJSON(b []byte) error {
	*v = false
	if isNull(b) {
		return nil
	}
	var bb bool
	if err := json.Unmarshal(b, &bb); err == nil {
		*v = flexBool(bb)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		bb, _ = strconv.ParseBool(strings.TrimSpace(s))
		*v = flexBool(bb)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*v = n != 0
	}
	return nil
}

// flexTime accepts RFC 3339 timestamps (with or without fractional seconds or
// a colon in the offset), Unix seconds/milliseconds, or null. Unparseable
// values decode to the zero time.
type flexTime struct{ T time.Time }

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
}

func (t *flexTime) UnmarshalJSON(b []byte) error {
	t.T = time.Time{}
	if isNull(b) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		t.T = ParseTime(s)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil && n > 0 {
		if n > 1e12 { // milliseconds
			t.T = time.UnixMilli(int64(n))
		} else {
			t.T = time.Unix(int64(n), 0)
		}
	}
	return nil
}

// ParseTime parses the timestamp formats seen in Anthropic APIs. It returns
// the zero time when s cannot be parsed.
func ParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// decodeObject decodes b into a map of raw fields. It returns false when b
// is not a JSON object.
func decodeObject(b []byte) (map[string]json.RawMessage, bool) {
	if isNull(b) {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

// decodeArray decodes b into raw elements, returning nil if b is not an array.
func decodeArray(b []byte) []json.RawMessage {
	if isNull(b) {
		return nil
	}
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil {
		return nil
	}
	return a
}

// nameOf extracts a human name from a value that may be a plain string or an
// object with display_name / name / id fields.
func nameOf(b []byte) string {
	if isNull(b) {
		return ""
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return strings.TrimSpace(s)
	}
	m, ok := decodeObject(b)
	if !ok {
		return ""
	}
	for _, k := range []string{"display_name", "name", "id", "key"} {
		var v flexString
		if raw, ok := m[k]; ok {
			_ = json.Unmarshal(raw, &v)
			if strings.TrimSpace(string(v)) != "" {
				return strings.TrimSpace(string(v))
			}
		}
	}
	return ""
}
