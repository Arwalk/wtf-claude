package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func fakeEnv(m map[string]string, tty bool) env {
	return env{getenv: func(k string) string { return m[k] }, isTTY: tty}
}

func TestOutputWidth(t *testing.T) {
	c := &config{}
	if w := outputWidth(c, fakeEnv(nil, false)); w != 60 {
		t.Errorf("default = %d", w)
	}
	if w := outputWidth(c, fakeEnv(map[string]string{"COLUMNS": "100"}, false)); w != 100 {
		t.Errorf("COLUMNS = %d", w)
	}
	// WTF_WIDGET_WIDTH is already the inner width (wtfutil subtracts the
	// border), so it is used as-is.
	if w := outputWidth(c, fakeEnv(map[string]string{"COLUMNS": "100", "WTF_WIDGET_WIDTH": "40"}, false)); w != 40 {
		t.Errorf("WTF = %d", w)
	}
	c.width = 25
	if w := outputWidth(c, fakeEnv(map[string]string{"WTF_WIDGET_WIDTH": "40"}, false)); w != 25 {
		t.Errorf("flag = %d", w)
	}
}

func TestOutputWidthFromTerminal(t *testing.T) {
	c := &config{}
	withTerm := func(m map[string]string, w int) env {
		e := fakeEnv(m, true)
		e.termWidth = w
		return e
	}
	cases := []struct {
		name string
		env  env
		want int
	}{
		{"narrow terminal", withTerm(nil, 50), 50},
		{"wide terminal capped", withTerm(nil, 200), maxAutoWidth},
		{"COLUMNS beats terminal", withTerm(map[string]string{"COLUMNS": "100"}, 50), 100},
		{"widget beats terminal", withTerm(map[string]string{"WTF_WIDGET_WIDTH": "40"}, 70), 40},
		{"unknown size", withTerm(nil, 0), 60},
	}
	for _, tc := range cases {
		if got := outputWidth(c, tc.env); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	c.width = 25
	if got := outputWidth(c, withTerm(nil, 70)); got != 25 {
		t.Errorf("flag: got %d, want 25", got)
	}
}

// A regular file is not a terminal: no size, no panic.
func TestTerminalWidthNotATTY(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if w := terminalWidth(f); w != 0 {
		t.Errorf("terminalWidth(regular file) = %d, want 0", w)
	}
}

func TestUseColor(t *testing.T) {
	cases := []struct {
		format, color string
		env           map[string]string
		tty, want     bool
	}{
		{"ansi", "auto", nil, false, false},
		{"ansi", "auto", nil, true, true},
		{"ansi", "auto", map[string]string{"WTF_WIDGET_WIDTH": "40"}, false, true},
		{"ansi", "auto", map[string]string{"NO_COLOR": "1"}, true, false},
		{"ansi", "always", map[string]string{"NO_COLOR": "1"}, false, true},
		{"ansi", "never", nil, true, false},
		{"plain", "always", nil, true, false},
	}
	for _, c := range cases {
		if got := useColor(&config{format: c.format, color: c.color}, fakeEnv(c.env, c.tty)); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestParseFlags(t *testing.T) {
	var stderr bytes.Buffer
	c, err := parseFlags([]string{"--sections", "plan,local", "--cache-ttl", "30", "--no-local"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !c.sections["plan"] || c.sections["local"] || c.sections["limits"] || c.usageTTL.Seconds() != 30 {
		t.Errorf("%+v", c)
	}
	c, _ = parseFlags([]string{"--compact", "--cache-ttl", "2m"}, &stderr)
	if !c.compact || len(c.sections) != 1 || !c.sections["limits"] || c.usageTTL.Minutes() != 2 {
		t.Errorf("%+v", c)
	}
	for _, bad := range [][]string{{"--format", "xml"}, {"--color", "maybe"}, {"--sections", "nope"}, {"--cache-ttl", "soon"}, {"extra"}} {
		if _, err := parseFlags(bad, &stderr); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

// setup creates an isolated HOME with credentials and a fake API server.
func setup(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-test-SECRET","expiresAt":4102444800000}}`), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("WTF_WIDGET_WIDTH", "")
	t.Setenv("NO_COLOR", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, home
}

// currentFixture returns testdata/usage.json with its recorded weekly window
// moved to one that is running now: run() uses the real clock, so the
// recorded dates would look like an expired window.
func currentFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	const layout = "2006-01-02T15:04:05"
	start := time.Now().UTC().AddDate(0, 0, -2)
	s := string(b)
	for recorded, current := range map[string]time.Time{
		"2026-11-15T10:00:00": start,                  // window start
		"2026-11-22T10:00:00": start.AddDate(0, 0, 7), // reset
	} {
		if !strings.Contains(s, recorded) {
			t.Fatalf("testdata/usage.json: no %s, update currentFixture", recorded)
		}
		s = strings.ReplaceAll(s, recorded, current.Format(layout))
	}
	return []byte(s)
}

func fixtureHandler(t *testing.T, calls *int32, status *int32) http.HandlerFunc {
	usage := currentFixture(t)
	profile, _ := os.ReadFile("../../testdata/profile.json")
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.Header.Get("Authorization") != "Bearer sk-test-SECRET" {
			w.WriteHeader(401)
			return
		}
		if s := atomic.LoadInt32(status); s != 200 {
			w.WriteHeader(int(s))
			return
		}
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.Write(usage)
		case "/api/oauth/profile":
			w.Write(profile)
		default:
			w.WriteHeader(404)
		}
	}
}

func runCmd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	if errb.Len() > 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

func TestEndToEnd(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	base := "--base-url=" + srv.URL

	out, code := runCmd(t, base, "--format", "plain", "--width", "60")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"Ultra plan · Max 20x", "Weekly · all models", "▸Weekly · Fable", "31% left", "Local Claude Code", "no activity"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "\x1b[") {
		t.Errorf("token or escapes leaked:\n%s", out)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	fi, err := os.Stat(filepath.Join(home, "cache", "claude-usage", "usage.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file: %v %v", fi, err)
	}

	// Cached: no further calls.
	runCmd(t, base, "--format", "plain")
	if calls != 2 {
		t.Errorf("cache not used, calls = %d", calls)
	}

	// Forced refresh during an outage: stale data + note, exit 0.
	atomic.StoreInt32(&status, 503)
	out, code = runCmd(t, base, "--format", "plain", "--cache-ttl", "0", "--no-local")
	if code != 0 || !strings.Contains(out, "stale · fetched") || !strings.Contains(out, "server error (503)") || !strings.Contains(out, "Weekly · Fable") {
		t.Errorf("stale run (exit %d):\n%s", code, out)
	}

	// JSON is valid and has the normalized limits.
	atomic.StoreInt32(&status, 200)
	out, _ = runCmd(t, base, "--format", "json", "--no-local")
	var rep struct {
		Limits []struct {
			Label       string  `json:"label"`
			PercentLeft float64 `json:"percent_left"`
		} `json:"limits"`
		Plan struct{ Display string } `json:"plan"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(rep.Limits) != 2 || rep.Limits[1].Label != "Weekly · Fable" || rep.Limits[1].PercentLeft != 31 || rep.Plan.Display == "" {
		t.Errorf("json report = %+v", rep)
	}

	// Inside wtfutil: width from WTF_WIDGET_WIDTH, colour on.
	t.Setenv("WTF_WIDGET_WIDTH", "40")
	out, _ = runCmd(t, base, "--compact")
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected colour in widget:\n%q", out)
	}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := len([]rune(stripANSI(l))); n > 40 {
			t.Errorf("line too wide (%d): %q", n, l)
		}
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("compact should be 2 lines, got %d:\n%s", n, out)
	}
}

func TestNoCacheAuthError(t *testing.T) {
	var calls, status int32 = 0, 401
	srv, _ := setup(t, fixtureHandler(t, &calls, &status))
	out, code := runCmd(t, "--base-url="+srv.URL, "--format", "plain", "--no-local", "--sections", "limits")
	if code != 1 || strings.TrimSpace(out) != "login expired · run claude" {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("error should be one line:\n%s", out)
	}
	// With local stats the run is still useful.
	out, code = runCmd(t, "--base-url="+srv.URL, "--format", "plain")
	if code != 0 || !strings.Contains(out, "Local Claude Code") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestNoCredentials(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	os.Remove(filepath.Join(home, ".claude", ".credentials.json"))
	out, _ := runCmd(t, "--base-url="+srv.URL, "--format", "plain")
	if !strings.Contains(out, "not logged in") || calls != 0 {
		t.Errorf("calls=%d:\n%s", calls, out)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestOutputHeight(t *testing.T) {
	c := &config{}
	if h := outputHeight(c, fakeEnv(nil, false)); h != 0 {
		t.Errorf("default = %d", h)
	}
	if h := outputHeight(c, fakeEnv(map[string]string{"WTF_WIDGET_HEIGHT": "12"}, false)); h != 12 {
		t.Errorf("WTF = %d", h)
	}
	c.height = 5
	if h := outputHeight(c, fakeEnv(map[string]string{"WTF_WIDGET_HEIGHT": "12"}, false)); h != 5 {
		t.Errorf("flag = %d", h)
	}
}

// Inside wtfutil the widget fits WTF_WIDGET_HEIGHT and keeps the binding
// limit.
func TestWidgetHeight(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, _ := setup(t, fixtureHandler(t, &calls, &status))
	t.Setenv("WTF_WIDGET_WIDTH", "60")
	t.Setenv("WTF_WIDGET_HEIGHT", "1")
	out, code := runCmd(t, "--base-url="+srv.URL, "--compact", "--color", "never")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "▸Weekly · Fable") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// cmdrunner discards stderr, so flag errors must also reach stdout there.
func TestFlagErrorInWidget(t *testing.T) {
	t.Setenv("WTF_WIDGET_WIDTH", "40")
	out, code := runCmd(t, "--sections", "limit,local")
	if code != 2 || !strings.Contains(out, `unknown section "limit"`) {
		t.Errorf("exit %d: %q", code, out)
	}
	t.Setenv("WTF_WIDGET_WIDTH", "")
	if out, _ := runCmd(t, "--format", "txt"); out != "" {
		t.Errorf("terminal: stdout should stay clean, got %q", out)
	}
}

// A successful response without limits is an answer, not a failure; and
// inside wtfutil a rendered error must not add "exit status 1".
func TestExitCodes(t *testing.T) {
	_, _ = setup(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/usage" {
			w.Write([]byte(`{"limits":[]}`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	out, code := runCmd(t, "--base-url="+srv.URL, "--compact", "--color", "never")
	if code != 0 || !strings.Contains(out, "no usage limits reported") {
		t.Errorf("no limits: exit %d %q", code, out)
	}
	t.Setenv("WTF_WIDGET_WIDTH", "40")
	out, code = runCmd(t, "--base-url=http://127.0.0.1:1", "--compact", "--cache-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "usage unavailable") {
		t.Errorf("widget error: exit %d %q", code, out)
	}
}

func TestAuthErrorWording(t *testing.T) {
	srv, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer handmade" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(403)
	})
	out, _ := runCmd(t, "--base-url="+srv.URL, "--sections", "limits", "--color", "never")
	if strings.TrimSpace(out) != "access denied (403)" {
		t.Errorf("403: %q", out)
	}
	out, _ = runCmd(t, "--base-url="+srv.URL, "--sections", "limits", "--token", "handmade", "--color", "never")
	if strings.TrimSpace(out) != "token rejected (401)" {
		t.Errorf("flag token 401: %q", out)
	}
}

// With cached data, an auth failure leads the output instead of hiding at
// the end of the stale note, also on runs in back-off that make no request.
func TestAuthErrorWithCache(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, _ := setup(t, fixtureHandler(t, &calls, &status))
	base := "--base-url=" + srv.URL
	if _, code := runCmd(t, base, "--no-local"); code != 0 {
		t.Fatal("warm-up failed")
	}
	atomic.StoreInt32(&status, 401)
	out, code := runCmd(t, base, "--no-local", "--color", "never", "--cache-ttl", "0")
	if code != 0 || !strings.HasPrefix(out, "login expired · run claude · data ") || !strings.Contains(out, "Weekly · Fable") || strings.Contains(out, "stale ·") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	before := atomic.LoadInt32(&calls)
	r := runJSON(t, base, "--cache-ttl", "0")
	if atomic.LoadInt32(&calls) != before {
		t.Error("request made during back-off")
	}
	if !r.Status.AuthError || !r.Status.Stale || r.Status.UsageError != "login expired · run claude" || len(r.Limits) != 2 {
		t.Errorf("in back-off: %+v", r.Status)
	}
}

// fakeClaude puts a `claude` script on PATH that counts its runs and, if
// newToken is set, stores it in the credentials file the way Claude Code
// does when it refreshes its login. It returns the number of runs so far.
func fakeClaude(t *testing.T, home, newToken string, exit int) func() int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >> %q\n", log)
	if newToken != "" {
		script += fmt.Sprintf("printf '%%s' '{\"claudeAiOauth\":{\"accessToken\":\"%s\",\"expiresAt\":4102444800000}}' > %q\n",
			newToken, filepath.Join(home, ".claude", ".credentials.json"))
	}
	script += fmt.Sprintf("exit %d\n", exit)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "run")
	}
}

// expireLogin replaces the stored token with one the fake API rejects.
func expireLogin(t *testing.T, home string) {
	t.Helper()
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-test-OLD","expiresAt":1000000000000}}`), 0o600)
}

type statusJSON struct {
	Limits []json.RawMessage `json:"limits"`
	Status struct {
		Stale             bool   `json:"stale"`
		UsageError        string `json:"usage_error"`
		AuthError         bool   `json:"auth_error"`
		LoginRefreshed    bool   `json:"login_refreshed"`
		LoginRefreshError string `json:"login_refresh_error"`
	} `json:"status"`
}

func runJSON(t *testing.T, args ...string) statusJSON {
	t.Helper()
	out, _ := runCmd(t, append(args, "--format", "json", "--no-local")...)
	var r statusJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	return r
}

func TestRefreshLogin(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	expireLogin(t, home)
	runs := fakeClaude(t, home, "sk-test-SECRET", 0)

	r := runJSON(t, "--base-url="+srv.URL, "--refresh-login")
	if len(r.Limits) != 2 || r.Status.Stale || r.Status.UsageError != "" || !r.Status.LoginRefreshed || r.Status.LoginRefreshError != "" {
		t.Errorf("after refresh: %+v", r)
	}
	// Usage and profile were both rejected, but claude ran only once.
	if n := runs(); n != 1 {
		t.Errorf("claude ran %d times, want 1", n)
	}
	if left, _ := os.ReadDir(os.Getenv("TMPDIR")); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
}

func TestRefreshLoginIsOptIn(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	expireLogin(t, home)
	runs := fakeClaude(t, home, "sk-test-SECRET", 0)
	base := "--base-url=" + srv.URL

	if r := runJSON(t, base); r.Status.UsageError != "login expired · run claude" || r.Status.LoginRefreshed {
		t.Errorf("without the flag: %+v", r)
	}
	// Claude Code cannot refresh a token passed in by hand.
	if r := runJSON(t, base, "--refresh-login", "--token", "handmade"); r.Status.UsageError != "token rejected (401)" {
		t.Errorf("--token: %+v", r)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "from-env")
	if r := runJSON(t, base, "--refresh-login"); r.Status.UsageError != "token rejected (401)" {
		t.Errorf("env token: %+v", r)
	}
	if n := runs(); n != 0 {
		t.Errorf("claude ran %d times, want 0", n)
	}
}

// A refresh that fails is not retried on every run.
func TestRefreshLoginBackoff(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	expireLogin(t, home)
	runs := fakeClaude(t, home, "", 1)
	base := "--base-url=" + srv.URL

	r := runJSON(t, base, "--refresh-login")
	if r.Status.UsageError != "login expired · run claude" || r.Status.LoginRefreshError != "claude failed (exit 1)" || r.Status.LoginRefreshed {
		t.Errorf("first run: %+v", r)
	}
	// Drop the API back-off so the next run requests (and is rejected) again.
	for _, name := range []string{"usage.json", "profile.json"} {
		os.Remove(filepath.Join(home, "cache", "claude-usage", name))
	}
	before := atomic.LoadInt32(&calls)
	r = runJSON(t, base, "--refresh-login")
	if atomic.LoadInt32(&calls) == before {
		t.Fatal("second run made no request")
	}
	if r.Status.UsageError != "login expired · run claude" || r.Status.LoginRefreshError != "claude failed (exit 1)" {
		t.Errorf("second run: %+v", r)
	}
	if n := runs(); n != 1 {
		t.Errorf("claude ran %d times, want 1", n)
	}
}

// Missing or unreadable credentials still show cached data, under the
// credential error.
func TestCredentialErrorUsesCache(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	base := "--base-url=" + srv.URL
	if _, code := runCmd(t, base, "--no-local", "--color", "never"); code != 0 {
		t.Fatal("warm-up failed")
	}
	credPath := filepath.Join(home, ".claude", ".credentials.json")
	// Ordered: the final check below expects the file to be missing.
	for _, step := range []struct {
		name   string
		mutate func()
	}{
		{"truncated", func() { os.WriteFile(credPath, nil, 0o600) }},
		{"missing", func() { os.Remove(credPath) }},
	} {
		name := step.name
		step.mutate()
		before := atomic.LoadInt32(&calls)
		out, code := runCmd(t, base, "--no-local", "--color", "never")
		first := strings.SplitN(out, "\n", 2)[0]
		if code != 0 || !strings.Contains(out, "Weekly · Fable") || !strings.Contains(out, "Ultra plan") ||
			!(strings.Contains(first, "credentials") || strings.Contains(first, "not logged in")) {
			t.Errorf("%s: exit %d:\n%s", name, code, out)
		}
		if atomic.LoadInt32(&calls) != before {
			t.Errorf("%s: API called without credentials", name)
		}
	}
	out, _ := runCmd(t, base, "--no-local", "--color", "never")
	if !strings.HasPrefix(out, "not logged in · run claude · data ") {
		t.Errorf("reason missing:\n%s", out)
	}
}

// Two widgets refreshing on the same tick make one request per endpoint.
func TestConcurrentRunsShareFetch(t *testing.T) {
	var calls, status int32 = 0, 200
	inner := fixtureHandler(t, &calls, &status)
	srv, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		inner(w, r)
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, errb bytes.Buffer
			run([]string{"--base-url=" + srv.URL, "--no-local", "--format", "plain"}, &out, &errb)
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("API calls = %d, want 2 (usage + profile once)", n)
	}
}

// With an explicit config dir only that dir's transcripts are counted.
func TestExplicitConfigDirLocalStats(t *testing.T) {
	_, home := setup(t, nil)
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	line := func(id string, out int) string {
		return `{"type":"assistant","timestamp":"` + ts + `","sessionId":"` + id + `","requestId":"r` + id + `","message":{"id":"m` + id + `","model":"claude-x","usage":{"output_tokens":` + strconv.Itoa(out) + `}}}` + "\n"
	}
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "p"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "p", "a.jsonl"), []byte(line("personal", 1000)), 0o600)
	work := filepath.Join(home, "work")
	os.MkdirAll(filepath.Join(work, "projects", "w"), 0o700)
	os.WriteFile(filepath.Join(work, "projects", "w", "b.jsonl"), []byte(line("work", 7)), 0o600)

	out, _ := runCmd(t, "--config-dir", work, "--sections", "local", "--format", "json")
	var rep struct {
		Local struct {
			Week struct {
				Tokens   struct{ Output int } `json:"tokens"`
				Sessions int                  `json:"sessions"`
			} `json:"week"`
		} `json:"local"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Local.Week.Tokens.Output != 7 || rep.Local.Week.Sessions != 1 {
		t.Errorf("work account stats = %+v", rep.Local.Week)
	}
	out, _ = runCmd(t, "--sections", "local", "--format", "json")
	json.Unmarshal([]byte(out), &rep)
	if rep.Local.Week.Tokens.Output != 1000 {
		t.Errorf("default account stats = %+v", rep.Local.Week)
	}
}

// A cached usage body describing a weekly window that has already ended
// must not be used as the local "week" (it would start before the scan).
func TestEndedWindowFallsBackToRollingWeek(t *testing.T) {
	now := time.Now().UTC()
	ws := now.Add(-(7*24 + 5) * time.Hour)
	body := fmt.Sprintf(`{"seven_day_breakdown":{"window_started_at":%q},"limits":[{"kind":"weekly_all","percent":50,"resets_at":%q}]}`,
		ws.Format(time.RFC3339), ws.Add(7*24*time.Hour).Format(time.RFC3339))
	srv, home := setup(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	tsOld := now.Add(-(7*24 + 3) * time.Hour).Format(time.RFC3339)
	tsNew := now.Add(-time.Hour).Format(time.RFC3339)
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "p"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "p", "a.jsonl"), []byte(
		`{"type":"assistant","timestamp":"`+tsOld+`","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"x","usage":{"output_tokens":1000000}}}`+"\n"+
			`{"type":"assistant","timestamp":"`+tsNew+`","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"x","usage":{"output_tokens":1}}}`+"\n"), 0o600)
	out, _ := runCmd(t, "--base-url="+srv.URL, "--format", "json", "--sections", "limits,local")
	var rep struct {
		Local struct {
			WeekFromAPI bool `json:"week_from_api"`
			Week        struct {
				Label string `json:"label"`
			} `json:"week"`
		} `json:"local"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Local.WeekFromAPI || rep.Local.Week.Label != "7d" {
		t.Errorf("week = %+v", rep.Local)
	}
}

// An unreadable transcript dir is reported in status.local_error.
func TestLocalErrorInJSON(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	_, home := setup(t, nil)
	locked := filepath.Join(home, ".claude", "projects", "locked")
	os.MkdirAll(locked, 0o700)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	out, _ := runCmd(t, "--sections", "local", "--format", "json")
	var rep struct {
		Status struct {
			LocalError string `json:"local_error"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Status.LocalError, "permission denied (") {
		t.Errorf("local_error = %q", rep.Status.LocalError)
	}
}

func TestShortErrKeepsUTF8(t *testing.T) {
	got := shortErr(errors.New(strings.Repeat("é", 100))) // 200 bytes
	if !utf8.ValidString(got) || len(got) > 120 || !strings.HasSuffix(got, "...") {
		t.Errorf("shortErr = %q (%d bytes)", got, len(got))
	}
}
