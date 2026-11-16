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
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-usage/internal/api"
	"claude-usage/internal/cache"
	"claude-usage/internal/creds"
	"claude-usage/internal/localstats"
	"claude-usage/internal/render"
	"claude-usage/internal/report"
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
	fs.SetOutput(stderr)
	sections := strings.Join(allSections, ",")
	fs.StringVar(&c.format, "format", "ansi", "output format: ansi, plain or json")
	fs.StringVar(&c.color, "color", "auto", "colour: auto, always or never (auto = TTY or inside wtfutil; NO_COLOR respected)")
	fs.IntVar(&c.width, "width", 0, "output width in cells (default: $WTF_WIDGET_WIDTH, $COLUMNS, or 60)")
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
	fs.StringVar(&c.baseURL, "base-url", api.DefaultBaseURL, "API base URL")
	fs.BoolVar(&c.showVer, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: claude-usage [flags]\n\nShows remaining Claude subscription usage and local Claude Code statistics.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
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

// env abstracts the process environment for testing.
type env struct {
	getenv func(string) string
	isTTY  bool
	now    func() time.Time
	loc    *time.Location
}

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
		fmt.Fprintln(stderr, "claude-usage:", err)
		if inWidget(e) {
			// cmdrunner discards stderr; without this the widget would
			// only say "exit status 2".
			fmt.Fprintln(stdout, "claude-usage:", err)
		}
		return 2
	}
	if c.showVer {
		fmt.Fprintln(stdout, "claude-usage", version)
		return 0
	}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		e.isTTY = true
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
		explicitToken := c.token != "" || e.getenv("CLAUDE_CODE_OAUTH_TOKEN") != ""
		cr, err := creds.Load(creds.Options{Token: c.token, ConfigDir: c.configDir})
		if err != nil {
			status.UsageError = shortErr(err)
			// The cache key of file/keychain credentials depends only on
			// the config dir, so data fetched before the credentials went
			// missing (logout, a rewrite in progress) can still be shown.
			if !explicitToken {
				suffix := cacheSuffix(configDir, false, "")
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
					Describe: describe, LockWait: c.timeout + 2*time.Second}
			}
			// The cache is keyed by config dir so different accounts
			// (CLAUDE_CONFIG_DIR) don't share data.
			suffix := cacheSuffix(configDir, explicitToken, cr.AccessToken)
			var apiWG sync.WaitGroup
			if needAPI {
				apiWG.Add(1)
				go func() {
					defer apiWG.Done()
					res := store.Get("usage"+suffix, now, policy(c.usageTTL, 30*time.Second, validateUsage), func() ([]byte, error) {
						return client.Get(ctx, api.UsagePath)
					})
					if res.Body != nil {
						usage, _ = api.ParseUsage(res.Body)
						t := res.FetchedAt
						status.UsageFetchedAt = &t
						status.UsageFromCache = res.FromCache
						status.Stale = res.Stale
					}
					if res.Err != nil {
						status.UsageError = errText(res.Err, describe)
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
						return client.Get(ctx, api.ProfilePath)
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

func shortErr(err error) string {
	msg := err.Error()
	if errors.Is(err, creds.ErrNotFound) {
		return "not logged in · run claude"
	}
	if len(msg) > 120 {
		msg = msg[:117] + "..."
	}
	return msg
}
