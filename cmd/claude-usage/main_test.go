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

	"github.com/Arwalk/wtf-claude/internal/api"
	"github.com/Arwalk/wtf-claude/internal/cache"
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
// Nothing of the caller's environment (home, keychain, widget or terminal
// size) is visible to the test.
func setup(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-test-SECRET","expiresAt":4102444800000}}`), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN",
		"WTF_WIDGET_WIDTH", "WTF_WIDGET_HEIGHT", "COLUMNS", "NO_COLOR"} {
		t.Setenv(k, "")
	}
	emptyKeychain(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, home
}

// emptyKeychain puts a `security` on PATH that finds no item, so that tests
// on macOS never read the developer's real login.
func emptyKeychain(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // no keychain lookup there, and no shell scripts
	}
	bin := t.TempDir()
	// 44 is errSecItemNotFound.
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// cacheFiles lists the cache entries for name; their full names depend on
// the test server's URL.
func cacheFiles(t *testing.T, home, name string) []string {
	t.Helper()
	dir := filepath.Join(home, "cache", "claude-usage")
	// Not filepath.Glob: the temp dir's own path may hold pattern characters.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), name) && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
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
	files := cacheFiles(t, home, "usage")
	if len(files) != 1 {
		t.Fatalf("cache files: %v", files)
	}
	fi, err := os.Stat(files[0])
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
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
	setup(t, nil) // an accepted command line would run for real
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
	for _, f := range append(cacheFiles(t, home, "usage"), cacheFiles(t, home, "profile")...) {
		os.Remove(f)
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
	// The back-off belongs to the login, whatever the server.
	if f := cacheFiles(t, home, "relogin"); len(f) != 1 || filepath.Base(f[0]) != "relogin.json" {
		t.Errorf("back-off entries: %v, want relogin.json", f)
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
	if unlock, err := (cache.Store{Dir: t.TempDir()}).Lock("probe", 0); err != nil {
		t.Skipf("no cross-process lock here: %v", err)
	} else {
		unlock()
	}
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

// A mistyped command line must not put the token on screen: cmdrunner shows
// stdout, and stderr often ends up in logs.
func TestFlagErrorsNeverEchoToken(t *testing.T) {
	setup(t, nil) // an accepted command line would run for real
	t.Setenv("WTF_WIDGET_WIDTH", "60")
	for _, args := range [][]string{
		{"--token", "=", "oat01-SECRET"},
		{"--token=", "oat01-SECRET"},
		{"---token=sk-ant-oat01-SECRET"},
		{"--tokensk-ant-oat01-SECRET"},
		{"--width", "sk-ant-oat01-SECRET"},
		{"--cache-ttl", "sk-ant-oat01-SECRET"},
		{"--format", "sk-ant-oat01-SECRET"},
		{"--sections", "sk-ant-oat01-SECRET"},
		{"--sections", "plan", "--format", "x-sk-ant-oat01-SECRET"},
		{"--width", "sk-ant-api03-ab_SECRET"},
		// A pasted token that was wrapped or folded.
		{"---token=sk-ant-oat01-HEAD\n  SECRET"},
		{"--token sk-ant-oat01-HEAD SECRET"},
		{"--width=sk-ant-oat01-HEAD\n  SECRET"},
		// A proxy's token need not look like Anthropic's.
		{"---token=SECRET"},
		{"--token SECRET"},
		{"--tokenSECRET"},
		{"--base-url", "https://user:oat01-SECRET@"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
		if strings.Contains(out.String()+errb.String(), "SECRET") {
			t.Errorf("%q echoed the token:\nstdout: %s\nstderr: %.200s", args, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "claude-usage: ") {
			t.Errorf("%q: no message in the widget: %q", args, out.String())
		}
	}
}

// The token goes to whatever --base-url names, so it must never travel
// unencrypted to another machine.
func TestBaseURLNeedsHTTPSOrLoopback(t *testing.T) {
	for _, bad := range []string{
		"http://api.example.invalid", "http://10.0.0.1", "http://127.0.0.1.example.invalid",
		"http://localhost.example.invalid", "http://127.0.0.1@api.example.invalid",
		"http://evillocalhost", "http://0.0.0.0", "",
		"ftp://127.0.0.1", "api.example.invalid", "https://", "http://", "https://api.example.invalid:port", "://",
	} {
		var stderr bytes.Buffer
		if _, err := parseFlags([]string{"--base-url", bad}, &stderr); err == nil || !strings.Contains(err.Error(), "--base-url") {
			t.Errorf("%q: err = %v, want a --base-url error", bad, err)
		}
	}
	for _, ok := range []string{
		"https://api.anthropic.com", "HTTPS://api.example.invalid:8443/prefix/", "http://127.0.0.1:8080",
		"http://127.9.9.9", "http://localhost:8080", "http://[::1]:8080",
	} {
		var stderr bytes.Buffer
		if c, err := parseFlags([]string{"--base-url", ok}, &stderr); err != nil || c.baseURL != ok {
			t.Errorf("%q: %+v, %v", ok, c, err)
		}
	}
}

// Data fetched from another --base-url is never shown as the real thing.
func TestCacheIsPerBaseURL(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, _ := setup(t, fixtureHandler(t, &calls, &status))
	if r := runJSON(t, "--base-url="+srv.URL); len(r.Limits) != 2 {
		t.Fatalf("warm-up: %+v", r)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer other.Close()
	if r := runJSON(t, "--base-url="+other.URL); len(r.Limits) != 0 {
		t.Errorf("limits from another server were shown: %+v", r)
	}
	before := atomic.LoadInt32(&calls)
	if r := runJSON(t, "--base-url="+srv.URL+"/"); len(r.Limits) != 2 || atomic.LoadInt32(&calls) != before {
		t.Errorf("trailing slash missed the cache: %+v", r)
	}
}

func TestHelpShowsRefreshLoginAsSwitch(t *testing.T) {
	setup(t, nil) // an accepted command line would run for real
	var out, errb bytes.Buffer
	if code := run([]string{"--help"}, &out, &errb); code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "-refresh-login\n") {
		t.Errorf("--refresh-login looks like it takes a value:\n%s", errb.String())
	}
}

// personalFixtureData is what testdata/profile.json says about the account
// holder; none of it may reach the disk.
var personalFixtureData = []string{"user@example.com", "Example User", "Example Org", "00000000-0000-4000-8000", "created_at", "invoice"}

func TestProfileCacheHoldsNoPersonalData(t *testing.T) {
	var calls, status int32 = 0, 200
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	args := []string{"--base-url=" + srv.URL, "--no-local", "--format", "plain", "--sections", "plan"}
	first, _ := runCmd(t, args...)
	var cached string
	for _, f := range cacheFiles(t, home, "") {
		b, _ := os.ReadFile(f)
		cached += string(b)
	}
	if !strings.Contains(cached, "default_claude_max_20x") {
		t.Fatalf("profile was not cached:\n%s", cached)
	}
	for _, s := range personalFixtureData {
		if strings.Contains(cached, s) {
			t.Errorf("cache contains %q", s)
		}
	}
	second, _ := runCmd(t, args...)
	if calls != 1 || second != first || !strings.Contains(second, "Ultra plan · Max 20x") {
		t.Errorf("from cache (%d calls): %q, first run: %q", calls, second, first)
	}
}

// Caches written by versions that stored the whole profile are cleaned up,
// for every account, without losing the plan.
func TestOldProfileCachesAreScrubbed(t *testing.T) {
	var calls, status int32 = 0, 503
	srv, home := setup(t, fixtureHandler(t, &calls, &status))
	profile, err := os.ReadFile("../../testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache", "claude-usage")
	store := cache.Store{Dir: dir}
	old := cache.Entry{FetchedAt: time.Now().Add(-48 * time.Hour), Body: profile, LastError: "kept"}
	for _, name := range []string{"profile", "profile-0badf00d"} {
		if err := store.Save(name, old); err != nil {
			t.Fatal(err)
		}
	}

	runCmd(t, "--base-url="+srv.URL, "--no-local", "--format", "plain", "--compact")
	for _, name := range []string{"profile", "profile-0badf00d"} {
		b, _ := os.ReadFile(filepath.Join(dir, name+".json"))
		for _, s := range personalFixtureData {
			if strings.Contains(string(b), s) {
				t.Errorf("%s still contains %q", name, s)
			}
		}
		e := store.Load(name)
		if !e.FetchedAt.Equal(old.FetchedAt) || e.LastError != "kept" || !strings.Contains(string(e.Body), "default_claude_max_20x") {
			t.Errorf("%s lost data: %+v", name, e)
		}
	}
}

// Scrubbing leaves entries that another process is refreshing and files that
// are not ours alone, and always gives the lock back.
func TestScrubProfiles(t *testing.T) {
	profile, err := os.ReadFile("../../testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	// The path must not be taken for a pattern.
	store := cache.Store{Dir: filepath.Join(t.TempDir(), "[x]", "c*d")}
	if runtime.GOOS == "windows" {
		store.Dir = filepath.Join(t.TempDir(), "[x]")
	}
	for _, name := range []string{"profile", "profile-notes"} {
		if err := store.Save(name, cache.Entry{FetchedAt: time.Now(), Body: profile}); err != nil {
			t.Fatal(err)
		}
	}
	personal := func(name string) bool { return strings.Contains(string(store.Load(name).Body), "user@example.com") }

	if unlock, err := store.Lock("profile", 0); err == nil {
		scrubProfiles(store)
		if !personal("profile") {
			t.Error("entry changed while another process held its lock")
		}
		unlock()
	}
	scrubProfiles(store)
	if personal("profile") {
		t.Error("entry not trimmed")
	}
	if !personal("profile-notes") {
		t.Error("a file that is not a cache entry was changed")
	}
	if unlock, err := store.Lock("profile", 0); errors.Is(err, cache.ErrLockTimeout) {
		t.Error("lock not released")
	} else if err == nil {
		unlock()
	}
}

// A failed profile request keeps its reason.
func TestProfileErrorIsReported(t *testing.T) {
	var calls, status int32 = 0, 503
	srv, _ := setup(t, fixtureHandler(t, &calls, &status))
	out, _ := runCmd(t, "--base-url="+srv.URL, "--no-local", "--sections", "plan", "--format", "json")
	if !strings.Contains(out, `"profile_error": "server error (503)"`) {
		t.Errorf("profile_error missing:\n%s", out)
	}
}

// Only another account or server changes the cache file names.
func TestCacheSuffix(t *testing.T) {
	_, home := setup(t, nil)
	def := filepath.Join(home, ".claude")
	for _, base := range []string{api.DefaultBaseURL, api.DefaultBaseURL + "/"} {
		if s := cacheSuffix(def, false, "", base); s != "" {
			t.Errorf("default dir on %s: %q, want none", base, s)
		}
	}
	seen := map[string]string{"": "default"}
	for name, s := range map[string]string{
		"other dir":          cacheSuffix("/work", false, "", api.DefaultBaseURL),
		"token":              cacheSuffix(def, true, "t", api.DefaultBaseURL),
		"other token":        cacheSuffix(def, true, "u", api.DefaultBaseURL),
		"other server":       cacheSuffix(def, false, "", "https://a.example.invalid"),
		"third server":       cacheSuffix(def, false, "", "https://b.example.invalid"),
		"other dir + server": cacheSuffix("/work", false, "", "https://a.example.invalid"),
		"token + server":     cacheSuffix(def, true, "t", "https://a.example.invalid"),
	} {
		if prev, dup := seen[s]; dup || len(s) != 9 {
			t.Errorf("%s: suffix %q (same as %s)", name, s, prev)
		}
		seen[s] = name
	}
}
