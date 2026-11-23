package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseUsageFixture(t *testing.T) {
	b, err := os.ReadFile("../../testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	u, err := ParseUsage(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Limits) != 2 {
		t.Fatalf("limits = %d, want 2", len(u.Limits))
	}
	l0, l1 := u.Limits[0], u.Limits[1]
	if l0.Kind != "weekly_all" || l0.Group != "weekly" || *l0.Percent != 45 || l0.IsActive || l0.ModelName != "" {
		t.Errorf("limit 0 = %+v", l0)
	}
	if l1.Kind != "weekly_scoped" || *l1.Percent != 69 || !l1.IsActive || l1.ModelName != "Fable" || l1.Severity != "normal" {
		t.Errorf("limit 1 = %+v", l1)
	}
	want := time.Date(2026, 11, 22, 10, 0, 0, 495934000, time.UTC)
	if !l0.ResetsAt.Equal(want) {
		t.Errorf("resets_at = %v, want %v", l0.ResetsAt, want)
	}
	if len(u.Legacy) != 1 || u.Legacy[0].Key != "seven_day" || u.Legacy[0].Utilization != 45 {
		t.Errorf("legacy = %+v", u.Legacy)
	}
	if u.ExtraUsage == nil || u.ExtraUsage.Enabled {
		t.Errorf("extra_usage = %+v", u.ExtraUsage)
	}
	if u.Spend == nil || u.Spend.Enabled || u.Spend.Used == nil || u.Spend.Used.Currency != "USD" {
		t.Errorf("spend = %+v", u.Spend)
	}
	if u.Breakdown == nil || len(u.Breakdown.Rows) != 4 || u.Breakdown.Rows[0].DisplayName != "Claude Code" || u.Breakdown.Rows[0].Percent != 99 {
		t.Errorf("breakdown = %+v", u.Breakdown)
	}
	if u.Breakdown.WindowStartedAt.IsZero() {
		t.Error("window_started_at not parsed")
	}
}

func TestParseUsageTolerant(t *testing.T) {
	cases := map[string]string{
		"empty object":     `{}`,
		"all null":         `{"limits":null,"five_hour":null,"seven_day":null,"extra_usage":null,"spend":null,"seven_day_breakdown":null}`,
		"limits not array": `{"limits":{"oops":true}}`,
		"garbage elements": `{"limits":[1,"x",null,[],{"kind":"weekly_all","percent":"12.5","resets_at":"not a date","scope":"weird","is_active":"true"}]}`,
		"unknown kind":     `{"limits":[{"kind":"monthly_moonbeam","group":null,"percent":3,"severity":null,"resets_at":null,"scope":{"model":"Opus","surface":{"display_name":"Cowork"}},"is_active":1}]}`,
		"spend weird":      `{"spend":{"used":{"amount_minor":"250","currency":"eur","exponent":null},"limit":"n/a","enabled":"true"}}`,
		"breakdown weird":  `{"seven_day_breakdown":{"rows":[{"key":"x"},{"display_name":"A","percent":5},7]}}`,
		"legacy only":      `{"five_hour":{"utilization":"80","resets_at":1763300000},"seven_day_opus":{"utilization":null},"seven_day_cowork":{"utilization":2}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			u, err := ParseUsage([]byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch name {
			case "garbage elements":
				if len(u.Limits) != 1 || *u.Limits[0].Percent != 12.5 || !u.Limits[0].ResetsAt.IsZero() || !u.Limits[0].IsActive {
					t.Errorf("got %+v", u.Limits)
				}
			case "unknown kind":
				l := u.Limits[0]
				if l.Kind != "monthly_moonbeam" || l.ModelName != "Opus" || l.Surface != "Cowork" || !l.IsActive {
					t.Errorf("got %+v", l)
				}
			case "spend weird":
				if !u.Spend.Enabled || u.Spend.Used.Value() != 2.5 || u.Spend.Used.Currency != "EUR" || u.Spend.Limit != nil {
					t.Errorf("got %+v / %+v", u.Spend, u.Spend.Used)
				}
			case "breakdown weird":
				if len(u.Breakdown.Rows) != 1 || u.Breakdown.Rows[0].DisplayName != "A" {
					t.Errorf("got %+v", u.Breakdown.Rows)
				}
			case "legacy only":
				if len(u.Legacy) != 2 || u.Legacy[0].Key != "five_hour" || u.Legacy[0].Utilization != 80 ||
					u.Legacy[0].ResetsAt.Unix() != 1763300000 || u.Legacy[1].Key != "seven_day_cowork" {
					t.Errorf("got %+v", u.Legacy)
				}
			}
		})
	}
	for _, bad := range []string{``, `null`, `[]`, `"x"`, `{"limits":`} {
		if _, err := ParseUsage([]byte(bad)); err == nil {
			t.Errorf("ParseUsage(%q) should fail", bad)
		}
	}
}

func TestParseProfileFixture(t *testing.T) {
	b, err := os.ReadFile("../../testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProfile(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanLabel != "Ultra plan" || p.PlanName != "Ultra" || p.RateLimitTier != "default_claude_max_20x" ||
		p.SubscriptionStatus != "active" || p.ExtraUsageEnabled || !p.HasClaudeMax {
		t.Errorf("profile = %+v", p)
	}
	if _, err := ParseProfile([]byte(`{"organization":null,"account":7}`)); err != nil {
		t.Errorf("tolerant profile: %v", err)
	}
}

func TestParseTime(t *testing.T) {
	for _, s := range []string{"2026-11-22T10:00:00.495934+00:00", "2026-11-22T10:00:00Z", "2026-11-22T10:00:00.495934", "2026-11-22T11:00:00+0100"} {
		got := ParseTime(s)
		if got.UTC().Format("2006-01-02T15:04") != "2026-11-22T10:00" {
			t.Errorf("ParseTime(%q) = %v", s, got)
		}
	}
	if !ParseTime("garbage").IsZero() {
		t.Error("garbage should be zero")
	}
}

func TestClientHeadersAndErrors(t *testing.T) {
	var gotAuth, gotBeta, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta, gotUA = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("User-Agent")
		switch r.URL.Path {
		case UsagePath:
			w.Write([]byte(`{"limits":[]}`))
		case "/429":
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(429)
		case "/401":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"secret details"}`))
		case "/403":
			w.WriteHeader(403)
			w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"scope requirement user:profile"}}`))
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok-123", UserAgent: "claude-usage/test"}
	body, err := c.Get(context.Background(), UsagePath)
	if err != nil || string(body) != `{"limits":[]}` {
		t.Fatalf("Get = %q, %v", body, err)
	}
	if gotAuth != "Bearer tok-123" || gotBeta != BetaHeader || gotUA != "claude-usage/test" {
		t.Errorf("headers: %q %q %q", gotAuth, gotBeta, gotUA)
	}

	_, err = c.Get(context.Background(), "/429")
	if RetryAfter(err) != 42*time.Second || Describe(err) != "rate limited (429)" {
		t.Errorf("429: %v %v %q", err, RetryAfter(err), Describe(err))
	}
	_, err = c.Get(context.Background(), "/401")
	if !IsAuthError(err) || Describe(err) != "login expired · run claude" {
		t.Errorf("401: %v %q", err, Describe(err))
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(Describe(err), "tok-123") {
		t.Error("error leaks body or token")
	}
	_, err = c.Get(context.Background(), "/boom")
	if Describe(err) != "server error (503)" {
		t.Errorf("503: %q", Describe(err))
	}
	// 403 is a permission/proxy problem, not an expired login.
	_, err = c.Get(context.Background(), "/403")
	if IsAuthError(err) || Describe(err) != "access denied (403)" {
		t.Errorf("403: %v %q", err, Describe(err))
	}
}

// A proxy that refuses the CONNECT for an https URL surfaces as the bare
// status text inside a *url.Error.
func TestDescribeProxyRefusal(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	c := &Client{
		BaseURL: "https://api.example.invalid",
		Token:   "tok",
		HTTP:    &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second},
	}
	_, err := c.Get(context.Background(), UsagePath)
	if err == nil || Describe(err) != "proxy refused (Forbidden)" {
		t.Errorf("err = %v, Describe = %q", err, Describe(err))
	}
}

func TestPercentStringWithSign(t *testing.T) {
	u, err := ParseUsage([]byte(`{"limits":[{"kind":"weekly_all","percent":"45%"},{"kind":"five_hour","percent":"n/a"}]}`))
	if err != nil || len(u.Limits) != 2 {
		t.Fatalf("%+v %v", u, err)
	}
	if u.Limits[0].Percent == nil || *u.Limits[0].Percent != 45 {
		t.Errorf("45%% = %v", u.Limits[0].Percent)
	}
	if u.Limits[1].Percent != nil {
		t.Errorf("n/a should be unknown, got %v", *u.Limits[1].Percent)
	}
}

func TestDescribeKeepsUTF8(t *testing.T) {
	got := Describe(errors.New(strings.Repeat("é", 50))) // 100 bytes
	if !utf8.ValidString(got) || len(got) > 60 || !strings.HasSuffix(got, "...") {
		t.Errorf("Describe = %q (%d bytes)", got, len(got))
	}
}

// A redirect would carry the bearer token along, even from https to plain
// http on the same host, so none is followed.
func TestClientRefusesRedirects(t *testing.T) {
	reached := false
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte(`{}`))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer secure.Close()

	hc := secure.Client()
	body, err := (&Client{HTTP: hc, BaseURL: secure.URL, Token: "tok"}).Get(context.Background(), UsagePath)
	if reached {
		t.Error("the redirect was followed")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusFound || body != nil {
		t.Errorf("body %q, err %v, want HTTP 302", body, err)
	}
	if got := Describe(err); got != "redirect refused (302)" {
		t.Errorf("Describe = %q", got)
	}
	if hc.CheckRedirect != nil {
		t.Error("the caller's http.Client was modified")
	}
}

// TrimProfile keeps exactly what ParseProfile reads.
func TestTrimProfile(t *testing.T) {
	b, err := os.ReadFile("../../testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"fixture": b,
		"odd types": []byte(`{"account":{"has_claude_max":"true","has_claude_pro":1,"email":"user@example.com"},
			"organization":{"plan_display_label":{"email":"user@example.com"},"plan_display_name":7,"rate_limit_tier":null,"has_extra_usage_enabled":"1"}}`),
		"empty": []byte(`{}`),
	} {
		trimmed, err := TrimProfile(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, _ := ParseProfile(body)
		got, err := ParseProfile(trimmed)
		if err != nil || *got != *want {
			t.Errorf("%s: parsed %+v (%v), want %+v", name, got, err, want)
		}
		for _, s := range []string{"user@example.com", "Example", "uuid", "created_at"} {
			if strings.Contains(string(trimmed), s) {
				t.Errorf("%s: kept %q: %s", name, s, trimmed)
			}
		}
		if again, _ := TrimProfile(trimmed); string(again) != string(trimmed) {
			t.Errorf("%s: not stable: %s then %s", name, trimmed, again)
		}
	}
	if _, err := TrimProfile([]byte(`[]`)); !errors.Is(err, ErrNotObject) {
		t.Errorf("array: %v", err)
	}
}
