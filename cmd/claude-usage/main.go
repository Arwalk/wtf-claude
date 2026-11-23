// Command claude-usage shows remaining Claude subscription usage and local
// Claude Code statistics. It is designed to run inside a wtfutil cmdrunner
// widget but works in any terminal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Arwalk/wtf-claude/internal/api"
	"github.com/Arwalk/wtf-claude/internal/cache"
	"github.com/Arwalk/wtf-claude/internal/creds"
	"github.com/Arwalk/wtf-claude/internal/localstats"
	"github.com/Arwalk/wtf-claude/internal/relogin"
	"github.com/Arwalk/wtf-claude/internal/render"
	"github.com/Arwalk/wtf-claude/internal/report"
)

// version is overridable with -ldflags "-X main.version=...".
var version = "0.1.0"

var allSections = []string{"plan", "limits", "breakdown", "spend", "local"}

type config struct {
	format     string
	color      string
	width      int
	height     int
	compact    bool
	sections   map[string]bool
	noLocal    bool
	usageTTL   time.Duration
	profileTTL time.Duration
	timeout    time.Duration
	token      string
	configDir  string
	cacheDir   string
	baseURL    string
	showVer    bool
	// refreshLogin lets a 401 run `claude -p` so Claude Code refreshes its
	// own login (see loginRefresher).
	refreshLogin bool
}

// durationFlag accepts "120" (seconds) or Go durations like "2m".
type durationFlag struct{ d *time.Duration }

func (f durationFlag) String() string {
	if f.d == nil {
		return ""
	}
	return f.d.String()
}

func (f durationFlag) Set(s string) error {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		*f.d = time.Duration(n) * time.Second
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use seconds or e.g. 2m)", s)
	}
	*f.d = d
	return nil
}

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	c := &config{usageTTL: 120 * time.Second, profileTTL: time.Hour, timeout: 10 * time.Second}
	fs := flag.NewFlagSet("claude-usage", flag.ContinueOnError)
	// The flag package prints its own errors, which quote the arguments;
	// run prints them instead, redacted.
	fs.SetOutput(io.Discard)
	sections := strings.Join(allSections, ",")
	fs.StringVar(&c.format, "format", "ansi", "output format: ansi, plain or json")
	fs.StringVar(&c.color, "color", "auto", "colour: auto, always or never (auto = TTY or inside wtfutil; NO_COLOR respected)")
	fs.IntVar(&c.width, "width", 0, "output width in cells (default: $WTF_WIDGET_WIDTH, $COLUMNS, the terminal width up to 80, or 60)")
	fs.IntVar(&c.height, "height", 0, "max output rows; less important lines are dropped to fit (default: $WTF_WIDGET_HEIGHT, else unlimited)")
	fs.BoolVar(&c.compact, "compact", false, "one line per limit, nothing else (for small widgets)")
	fs.StringVar(&sections, "sections", sections, "comma-separated sections to show: "+strings.Join(allSections, ","))
	fs.BoolVar(&c.noLocal, "no-local", false, "skip scanning local Claude Code transcripts")
	fs.Var(durationFlag{&c.usageTTL}, "cache-ttl", "max age of cached usage data, e.g. 120 or 2m (0 = always refetch)")
	fs.Var(durationFlag{&c.profileTTL}, "profile-ttl", "max age of cached plan/profile data")
	fs.Var(durationFlag{&c.timeout}, "timeout", "HTTP timeout")
	fs.StringVar(&c.token, "token", "", "OAuth access token (default: $CLAUDE_CODE_OAUTH_TOKEN, keychain, or credentials file)")
	fs.StringVar(&c.configDir, "config-dir", "", "Claude config dir (default: $CLAUDE_CONFIG_DIR or ~/.claude)")
	fs.StringVar(&c.cacheDir, "cache-dir", "", "cache dir (default: $XDG_CACHE_HOME/claude-usage or ~/.cache/claude-usage)")
	fs.StringVar(&c.baseURL, "base-url", api.DefaultBaseURL, "API base URL, for testing or a proxy; it receives your token (https, or http to localhost only)")
	fs.BoolVar(&c.refreshLogin, "refresh-login", false, "when the stored login is rejected, run \"claude -p\" (Haiku, one-word reply) so Claude Code refreshes it, then retry")
	fs.BoolVar(&c.showVer, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: claude-usage [flags]\n\nShows remaining Claude subscription usage and local Claude Code statistics.\n\nFlags:\n")
		fs.SetOutput(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		// Not quoted: after a stray space or "=" this is the token.
		return nil, errors.New("unexpected argument (all options start with --; see --help)")
	}
	if err := checkBaseURL(c.baseURL); err != nil {
		return nil, err
	}
	switch c.format {
	case "ansi", "plain", "json":
	case "text":
		c.format = "plain"
	default:
		return nil, fmt.Errorf("invalid --format %q (ansi, plain or json)", c.format)
	}
	switch c.color {
	case "auto", "always", "never":
	default:
		return nil, fmt.Errorf("invalid --color %q (auto, always or never)", c.color)
	}
	c.sections = map[string]bool{}
	for _, s := range strings.Split(sections, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if s == "all" {
			for _, a := range allSections {
				c.sections[a] = true
			}
			continue
		}
		ok := false
		for _, a := range allSections {
			ok = ok || a == s
		}
		if !ok {
			return nil, fmt.Errorf("unknown section %q (valid: %s)", s, strings.Join(allSections, ","))
		}
		c.sections[s] = true
	}
	if c.noLocal {
		delete(c.sections, "local")
	}
	if c.compact {
		// Compact mode shows limits only.
		c.sections = map[string]bool{"limits": true}
	}
	if c.timeout <= 0 {
		c.timeout = 10 * time.Second
	}
	return c, nil
}

// checkBaseURL accepts only servers the token can be sent to without
// travelling unencrypted over a network. The error does not quote the URL,
// which may hold a password.
func checkBaseURL(s string) error {
	if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
		switch host := u.Hostname(); u.Scheme {
		case "https":
			return nil
		case "http":
			if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
				return nil
			}
		}
	}
	return errors.New("invalid --base-url (must be https, or http to localhost)")
}

// tokenLike matches Anthropic tokens and API keys, and whatever follows a
// misplaced --token (a proxy's token can look like anything). It runs to the
// end of the message: a pasted token may be wrapped or folded.
var tokenLike = regexp.MustCompile(`(?is)sk-ant.*|-token.+`)

// redact removes anything that looks like a token from a message about the
// command line, which quotes what was typed.
func redact(msg string) string { return tokenLike.ReplaceAllString(msg, "[redacted]") }

// env abstracts the process environment for testing.
type env struct {
	getenv func(string) string
	isTTY  bool
	// termWidth is the terminal's column count when stdout is a TTY (0 = unknown).
	termWidth int
	now       func() time.Time
	loc       *time.Location
}

// maxAutoWidth caps a width taken from the terminal size, so a very wide
// terminal doesn't stretch the bars across the whole screen. Explicit
// widths (--width, WTF_WIDGET_WIDTH, COLUMNS) are not capped.
const maxAutoWidth = 80

func outputWidth(c *config, e env) int {
	if c.width > 0 {
		return c.width
	}
	// wtfutil already subtracts the border: WTF_WIDGET_WIDTH is the inner
	// width, and text widgets don't wrap, so all of it is usable.
	if v, err := strconv.Atoi(strings.TrimSpace(e.getenv("WTF_WIDGET_WIDTH"))); err == nil && v > 0 {
		return v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(e.getenv("COLUMNS"))); err == nil && v > 0 {
		return v
	}
	if e.termWidth > 0 {
		return min(e.termWidth, maxAutoWidth)
	}
	return 60
}

// outputHeight is the number of rows available (0 = unlimited). Like the
// width, WTF_WIDGET_HEIGHT is the widget's inner height.
func outputHeight(c *config, e env) int {
	if c.height > 0 {
		return c.height
	}
	if v, err := strconv.Atoi(strings.TrimSpace(e.getenv("WTF_WIDGET_HEIGHT"))); err == nil && v > 0 {
		return v
	}
	return 0
}

// inWidget reports whether we run inside a wtfutil cmdrunner widget, which
// shows stdout only and appends "exit status N" for a non-zero exit.
func inWidget(e env) bool { return e.getenv("WTF_WIDGET_WIDTH") != "" }

func useColor(c *config, e env) bool {
	switch {
	case c.format != "ansi" || c.color == "never":
		return false
	case c.color == "always":
		return true
	case e.getenv("NO_COLOR") != "":
		return false
	}
	return e.isTTY || e.getenv("WTF_WIDGET_WIDTH") != ""
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	e := env{getenv: os.Getenv, now: time.Now, loc: time.Local}
	c, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		msg := redact(err.Error())
		fmt.Fprintln(stderr, "claude-usage:", msg)
		if inWidget(e) {
			// cmdrunner discards stderr; without this the widget would
			// only say "exit status 2".
			fmt.Fprintln(stdout, "claude-usage:", msg)
		}
		return 2
	}
	if c.showVer {
		fmt.Fprintln(stdout, "claude-usage", version)
		return 0
	}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		e.isTTY = true
		e.termWidth = terminalWidth(os.Stdout)
	}
	rep := collect(c, e)
	if c.format == "json" {
		if err := render.JSON(stdout, rep); err != nil {
			return 1
		}
	} else {
		opt := render.Options{Width: outputWidth(c, e), Height: outputHeight(c, e), Color: useColor(c, e), Compact: c.compact, Loc: e.loc}
		if err := render.Text(stdout, rep, opt); err != nil {
			return 1
		}
	}
	// Inside wtfutil the rendered message is the whole story; a non-zero
	// exit would only add an "exit status 1" line to the widget.
	if !useful(rep) && !inWidget(e) {
		return 1
	}
	return 0
}

// useful reports whether anything beyond an error message was rendered.
// A successful response that reports no limits counts: it is an answer.
func useful(r *report.Report) bool {
	return len(r.Limits) > 0 || r.Plan != nil || r.Local != nil || r.Breakdown != nil || r.Spend != nil || r.Status.NoLimitData
}

// collect gathers API and local data concurrently and builds the report.
func collect(c *config, e env) *report.Report {
	now := e.now()
	needAPI := c.sections["limits"] || c.sections["breakdown"] || c.sections["spend"]
	needProfile := c.sections["plan"]
	needLocal := c.sections["local"]

	var (
		status  report.Status
		usage   *api.Usage
		profile *api.Profile
		scan    localstats.ScanResult
		scanErr error
		wg      sync.WaitGroup
	)
	credOpts := creds.Options{ConfigDir: c.configDir}
	configDir := creds.ConfigDir(credOpts)

	// The scan covers the rolling 7 days (and today); report.WeekStart only
	// ever returns a start within the last 7 days, so this covers it too.
	since := now.Add(-7 * 24 * time.Hour)
	if sod := localstats.StartOfDay(now, e.loc); sod.Before(since) {
		since = sod
	}
	since = since.Add(-time.Hour)
	if needLocal {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scan, scanErr = localstats.Scan(localstats.DefaultDirs(configDir, creds.ConfigDirExplicit(credOpts)), since)
		}()
	}

	if needAPI || needProfile {
		dir := c.cacheDir
		if dir == "" {
			dir = cache.DefaultDir()
		}
		store := cache.Store{Dir: dir}
		scrubProfiles(store)
		explicitToken := c.token != "" || e.getenv("CLAUDE_CODE_OAUTH_TOKEN") != ""
		cr, err := creds.Load(creds.Options{Token: c.token, ConfigDir: c.configDir})
		if err != nil {
			status.UsageError = shortErr(err)
			status.AuthError = true
			// The cache key of file/keychain credentials depends only on
			// the config dir, so data fetched before the credentials went
			// missing (logout, a rewrite in progress) can still be shown.
			if !explicitToken {
				suffix := cacheSuffix(configDir, false, "", c.baseURL)
				if needAPI {
					if ce := store.Load("usage" + suffix); ce.HasBody() {
						if u, perr := api.ParseUsage(ce.Body); perr == nil {
							usage = u
							t := ce.FetchedAt
							status.UsageFetchedAt = &t
							status.UsageFromCache = true
							status.Stale = true
						}
					}
				}
				if needProfile {
					if ce := store.Load("profile" + suffix); ce.HasBody() {
						profile, _ = api.ParseProfile(ce.Body)
					}
				}
			}
		} else {
			status.CredentialSource = cr.Source
			client := &api.Client{
				HTTP:      &http.Client{Timeout: c.timeout},
				BaseURL:   c.baseURL,
				Token:     cr.AccessToken,
				UserAgent: "claude-usage/" + version,
			}
			ctx, cancel := context.WithTimeout(context.Background(), c.timeout+2*time.Second)
			defer cancel()
			describe := func(err error) string {
				if api.IsAuthError(err) && (cr.Source == "flag" || cr.Source == "env") {
					// `claude` cannot refresh a token passed in by hand.
					return "token rejected (401)"
				}
				return api.Describe(err)
			}
			policy := func(ttl, backoff time.Duration, validate func([]byte) error) cache.Policy {
				return cache.Policy{TTL: ttl, Backoff: backoff, RetryAfter: api.RetryAfter, Validate: validate,
					Describe: describe, IsAuth: api.IsAccessError, LockWait: c.timeout + 2*time.Second}
			}
			// The cache is keyed by config dir so different accounts
			// (CLAUDE_CONFIG_DIR) don't share data.
			suffix := cacheSuffix(configDir, explicitToken, cr.AccessToken, c.baseURL)
			var refresher *loginRefresher
			if c.refreshLogin && cr.Source != "flag" && cr.Source != "env" {
				// Only an explicit config dir is passed on: Claude Code
				// treats a set CLAUDE_CONFIG_DIR as another account.
				reloginDir := ""
				if creds.ConfigDirExplicit(credOpts) {
					reloginDir = configDir
				}
				// The login is the same whatever the base URL, so refreshes
				// are keyed by config dir alone.
				refresher = &loginRefresher{
					store: store, name: "relogin" + cacheSuffix(configDir, false, "", api.DefaultBaseURL), now: now,
					load: func() (creds.Credentials, error) { return creds.Load(credOpts) },
					run: func() error {
						return relogin.Run(context.Background(), relogin.Options{ConfigDir: reloginDir, Timeout: reloginTimeout})
					},
				}
			}
			// get fetches path; with --refresh-login, a 401 gets the login
			// refreshed and the request retried once with the new token.
			get := func(path string) func() ([]byte, error) {
				return func() ([]byte, error) {
					body, err := client.Get(ctx, path)
					if refresher == nil || !api.IsAuthError(err) {
						return body, err
					}
					tok, rerr := refresher.refresh(cr.AccessToken)
					if rerr != nil {
						return body, err
					}
					// ctx may have expired while claude ran.
					rctx, rcancel := context.WithTimeout(context.Background(), c.timeout+2*time.Second)
					defer rcancel()
					retry := *client
					retry.Token = tok
					return retry.Get(rctx, path)
				}
			}
			var apiWG sync.WaitGroup
			if needAPI {
				apiWG.Add(1)
				go func() {
					defer apiWG.Done()
					res := store.Get("usage"+suffix, now, policy(c.usageTTL, 30*time.Second, validateUsage), get(api.UsagePath))
					if res.Body != nil {
						usage, _ = api.ParseUsage(res.Body)
						t := res.FetchedAt
						status.UsageFetchedAt = &t
						status.UsageFromCache = res.FromCache
						status.Stale = res.Stale
					}
					if res.Err != nil {
						status.UsageError = errText(res.Err, describe)
						status.AuthError = isAuthErr(res.Err)
					}
				}()
			}
			if needProfile {
				apiWG.Add(1)
				go func() {
					defer apiWG.Done()
					// Plan data is optional (and some tokens lack the profile
					// scope), so failures back off for longer.
					res := store.Get("profile"+suffix, now, policy(c.profileTTL, 10*time.Minute, validateProfile), func() ([]byte, error) {
						body, err := get(api.ProfilePath)()
						if err != nil {
							return body, err
						}
						return api.TrimProfile(body)
					})
					if res.Body != nil {
						profile, _ = api.ParseProfile(res.Body)
					}
					if res.Err != nil {
						status.ProfileError = errText(res.Err, describe)
					}
				}()
			}
			apiWG.Wait()
			if refresher != nil {
				if tried, err := refresher.result(); tried && err != nil {
					status.LoginRefreshError = err.Error()
				} else if tried {
					status.LoginRefreshed = true
				}
			}
		}
	}
	wg.Wait()

	var local *localstats.Stats
	if needLocal {
		weekStart := report.WeekStart(usage, now)
		if weekStart.Before(since) {
			weekStart = time.Time{} // never claim a period that was not scanned
		}
		st := localstats.Compute(scan.Entries, now, e.loc, weekStart)
		st.FilesScanned, st.LinesSkipped = scan.FilesScanned, scan.LinesSkipped
		local = &st
		if scanErr != nil {
			status.LocalError = shortErr(scanErr)
		}
	}

	rep := report.Build(report.Input{Version: version, Now: now, Usage: usage, Profile: profile, Local: local, Status: status})
	if !c.sections["limits"] {
		rep.Limits = nil
	}
	if !c.sections["breakdown"] {
		rep.Breakdown = nil
	}
	if !c.sections["spend"] {
		rep.Spend = nil
	}
	if !c.sections["plan"] {
		rep.Plan = nil
	}
	return rep
}

func validateUsage(b []byte) error {
	_, err := api.ParseUsage(b)
	return err
}

func validateProfile(b []byte) error {
	_, err := api.ParseProfile(b)
	return err
}

func errText(err error, describe func(error) string) string {
	var be *cache.ErrBackoff
	if errors.As(err, &be) {
		return be.Error()
	}
	return describe(err)
}

// isAuthErr reports whether a fetch failed because of the login or access
// rights, including a run in back-off after such a failure.
func isAuthErr(err error) bool {
	var be *cache.ErrBackoff
	if errors.As(err, &be) {
		return be.Auth
	}
	return api.IsAccessError(err)
}

func shortErr(err error) string {
	msg := err.Error()
	if errors.Is(err, creds.ErrNotFound) {
		return "not logged in · run claude"
	}
	if len(msg) > 120 {
		msg = cutAtRune(msg, 117) + "..."
	}
	return msg
}

// cutAtRune cuts s to at most n bytes without splitting a UTF-8 character.
func cutAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
