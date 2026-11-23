# claude-usage

A small command-line tool that shows how much of your Claude subscription
usage is left, plus statistics from your local Claude Code sessions. It is
meant to run inside a [wtfutil](https://wtfutil.com) dashboard through
wtfutil's built-in `cmdrunner` module, and it works just as well in a plain
terminal.

```
Ultra plan · Max 20x
 Weekly · all models ██████████████░░░░░░░░░░░░░░░░ 54% left
  resets in 5d 16h (Sun 10:00)
  28% over pace · runs out in 1d 12h (Wed 05:30)
▸Weekly · Fable      █████████████████████░░░░░░░░░ 29% left
  resets in 5d 16h (Sun 10:00)
  53% over pace · runs out in 12h 41m (Tue 05:44)
By surface: Claude Code 99% · Cowork 1%
Local Claude Code
Today  4.2M tok · 112k out · 42 msg · 1 sess
Week   4.2M tok · 112k out · 42 msg · 1 sess
Models opus-5-5 100%
```

With `--compact`:

```
 Weekly · all models ███████████░░░░░░░░░░░░ 54% left ↻5d16h
▸Weekly · Fable      ████████████████░░░░░░░ 29% left ↻5d16h
```

## What it shows

| Section     | Contents |
|-------------|----------|
| `plan`      | Plan label and rate-limit tier, e.g. `Ultra plan · Max 20x`. A subscription status other than `active` is appended. Email and name are never shown. |
| `limits`    | One row per rate-limit window: a bar showing the share **used**, the share **left** (rounded down, so `<1% left` until the limit is actually reached), and below it the reset countdown with the local wall-clock time. `▸` marks the limit that is binding right now. Colours follow the number shown: green above 30% left, yellow down to 10% left, red at 10% left or less, i.e. green below 70% used, yellow below 90%, red at 90% or more (a `warning`/`critical` severity from the API raises the colour). A limit whose value the API sent as null or unreadable shows `? left` with an empty grey bar and no pace. A limit whose reset time has already passed (stale cached data) shows `reset` instead of its old percentage, with `reset · refreshing` below it; when the refresh failed, that line is left out. |
| pace        | For each limit with a known window (weekly = 7 days, session = 5 hours): how far usage is ahead of or behind an even pace, and, if the current average rate would hit 100% before the reset, when it runs out. No projection is made in the first 3% of a window. |
| `breakdown` | The 7-day usage split by surface (Claude Code, chats, Cowork, ...), showing only surfaces above 0%. |
| `spend`     | Extra (pay-as-you-go) usage, shown only when it is enabled: amount spent, the monthly limit and the percentage used. |
| `local`     | Totals from Claude Code transcripts on this machine for **today** (local timezone) and the **current weekly window** (the subscription window when the API reports it, otherwise the last 7 days): total tokens (input + output + cache creation + cache read), output tokens, messages, sessions, and the top models by tokens. If transcripts can't be read and nothing could be counted, it says so instead of reporting no activity. |

Limit labels: `weekly_all` is shown as *Weekly · all models*, `weekly_scoped`
as *Weekly · \<model or surface\>*, and five-hour or session kinds as
*Session (5h)*. Kinds the tool doesn't recognise are shown with a readable
version of their name. When the full labels don't fit (narrow or compact
widgets), short labels are used instead: *All*, the model or surface name
(e.g. *Fable*), and *5h*. If the API returns no `limits` array, the tool falls
back to the older `five_hour` / `seven_day` / `seven_day_*` objects.

## Install

Requires Go 1.22 or newer. There are no third-party dependencies.

```sh
go install github.com/Arwalk/wtf-claude/cmd/claude-usage@latest   # installs to $(go env GOPATH)/bin
```

Or from a clone:

```sh
git clone https://github.com/Arwalk/wtf-claude && cd wtf-claude
go install ./cmd/claude-usage
# or
go build -o claude-usage ./cmd/claude-usage
```

You must be logged in to Claude Code with a Claude subscription (`claude`,
then `/login`). API-key logins have no subscription usage to show.

## Usage

```
claude-usage [flags]

  --format ansi|plain|json   output format (default ansi)
  --color auto|always|never  auto = colour when stdout is a TTY or when running
                             inside wtfutil (WTF_WIDGET_WIDTH is set);
                             NO_COLOR disables auto colour
  --width N                  width in cells (default: $WTF_WIDGET_WIDTH,
                             then $COLUMNS, then the terminal width up to
                             80, then 60)
  --height N                 max rows; less important lines are dropped to
                             fit (default: $WTF_WIDGET_HEIGHT, else unlimited)
  --compact                  limits only, one line each (for small widgets)
  --sections LIST            comma-separated: plan,limits,breakdown,spend,local
                             (default: all)
  --no-local                 don't scan local Claude Code transcripts
  --cache-ttl D              max age of cached usage data, e.g. 120 or 2m
                             (default 120s; 0 = always refetch)
  --profile-ttl D            max age of cached plan data (default 1h)
  --timeout D                HTTP timeout (default 10s)
  --token TOKEN              OAuth access token to use instead of the stored one
  --config-dir DIR           Claude config dir (default $CLAUDE_CONFIG_DIR or ~/.claude)
  --cache-dir DIR            cache dir (default $XDG_CACHE_HOME/claude-usage
                             or ~/.cache/claude-usage)
  --refresh-login            when the stored login is rejected (401), run
                             `claude -p` so Claude Code refreshes it, then
                             retry (see "How it works")
  --version
```

Examples:

```sh
claude-usage                                  # full view in the terminal
claude-usage --compact
claude-usage --sections limits,local --width 40
claude-usage --format json | jq '.limits[] | {label, percent_left}'
```

The layout adapts to the width. Below about 40 cells, the reset and pace
details wrap onto separate lines and wording gets shorter. When a label no
longer fits next to its bar, the full layout moves the bar under the label
and the compact layout switches to short labels. Lines are never wider than
the requested width (the minimum is 12).

With a height (`--height`, or `WTF_WIDGET_HEIGHT` inside wtfutil), the full
view drops less important lines until it fits, in this order: stacked rows
(short labels side by side instead), the Models line, the second reset/pace
line of each limit, the surface breakdown, the local-stats header, all
reset/pace lines, the local stats, and finally plan and extra usage. If it
still does not fit, the compact view is used. The compact view keeps the
binding limit and then the most used ones, plus the stale note when there is
room, and shows `+N more` when it can spare a row for it. A login or access
error (see "How it works") is always kept, as the first line, even when only
one row is available.

The exit status is 0 whenever something useful was printed, even if it was
only local stats, stale data or a response with no limits. It is 1 when
nothing but an error could be shown, except inside wtfutil
(`WTF_WIDGET_WIDTH` set), where the error message is the widget's content
and the exit status is 0 so wtfutil does not add an `exit status 1` line.
Invalid flags exit with 2; inside wtfutil the error is also printed to
stdout, because cmdrunner discards stderr.

### wtfutil

Use the built-in `cmdrunner` module. A complete minimal config with a full
widget and a compact widget is in
[`examples/wtf-config.yml`](examples/wtf-config.yml):

```yaml
wtf:
  grid:
    columns: [62]
    rows: [17, 5]
  refreshInterval: 1
  mods:
    claude:
      type: cmdrunner
      title: "Claude"
      cmd: "claude-usage"          # or a full path, e.g. /home/you/go/bin/claude-usage
      args: ["--cache-ttl", "110"]
      enabled: true
      position: { top: 0, left: 0, height: 1, width: 1 }
      refreshInterval: 120
    claude_compact:
      type: cmdrunner
      title: "Claude"
      cmd: "claude-usage"
      args: ["--compact", "--cache-ttl", "110"]
      enabled: true
      position: { top: 1, left: 0, height: 1, width: 1 }
      refreshInterval: 120
```

* `cmd` must be on wtfutil's `PATH` or a full path. Only wtfutil 0.51 and
  newer expand `~` in `cmd`; older versions fail with
  `fork/exec ~/go/bin/claude-usage: no such file or directory`.
* `cmdrunner` passes the widget's inner size (border already subtracted) in
  `WTF_WIDGET_WIDTH` and `WTF_WIDGET_HEIGHT`. The tool uses the full width,
  fits its output to the height and turns on ANSI colour by itself.
  `cmdrunner` turns ANSI escape codes into tview colours, so you don't need
  `pty: true`.
* Sizing: the grid row height includes the 2 border rows. The full view needs
  about 12 inner rows for two limits with all sections; each additional limit
  (for example a 5-hour session limit) needs up to 3 more, and a stale note or
  extra usage 1 each. With fewer rows, details are dropped as described
  above rather than cut off. The compact view needs one row per limit plus
  one for a stale note. Use `--sections` to leave out what you don't need.
* Set `--cache-ttl` slightly below `refreshInterval` so that each refresh
  gets fresh numbers. Widgets and terminal runs share the same cache, and
  refreshes are serialised with a lock file, so several widgets refreshing
  at the same moment make one request.

### JSON output

`--format json` prints a normalized document, which is a stable interface for
scripts (fields are omitted when unknown):

```jsonc
{
  "version": "0.1.0",
  "generated_at": "2026-11-16T17:03:08Z",
  "plan": { "label": "Ultra plan", "name": "Ultra", "tier": "Max 20x",
            "rate_limit_tier": "default_claude_max_20x",
            "subscription_status": "active", "extra_usage_enabled": false,
            "display": "Ultra plan · Max 20x" },
  "limits": [{
    "kind": "weekly_scoped", "label": "Weekly · Fable", "short_label": "Fable",
    "group": "weekly",
    "source": "limits",              // or "legacy" (five_hour/seven_day objects)
    "percent_used": 71, "percent_left": 29,   // omitted when unknown (null or
                                     // unreadable in the API, or expired)
    "severity": "warning",           // ok | warning | critical | unknown
    "api_severity": "normal",        // raw value from the API
    "active": true,                  // the currently binding limit
    "expired": false,                // resets_at has passed: data predates the reset
                                     // (only present when true)
    "resets_at": "2026-11-22T10:00:00Z", "resets_in_seconds": 493012,  // never negative
    "window_seconds": 604800, "window_start": "2026-11-15T10:00:00Z",
    "pace": {
      "elapsed_fraction": 0.1848, "expected_percent": 18.5,
      "delta_percent": 52.5,         // used - expected; > 0 means faster than even
      "status": "over",              // under | on_track (within ±5) | over
      "projected_percent": 384.1,    // at reset, if the average rate continues
      "runs_out_at": "2026-11-17T05:44:00Z"
    }
  }],
  "breakdown": { "window_started_at": "...", "as_of": "...",
                 "rows": [{ "key": "claude_code", "name": "Claude Code", "percent": 99 }] },
  "spend": { "source": "spend", "currency": "USD", "used": 12.34, "limit": 50,
             "percent": 24.7, "severity": "ok" },          // only when enabled
  "local": {
    "today": { "label": "today", "start": "...", "end": "...",
               "tokens": { "input": 0, "output": 0, "cache_creation": 0, "cache_read": 0, "total": 0 },
               "messages": 0, "sessions": 0,
               "models": [{ "model": "claude-opus-5-5", "tokens": { ... }, "messages": 0, "share": 1 }] },
    "week": { ... },
    "week_from_api": true,           // week = subscription window, else rolling 7 days
    "files_scanned": 3, "lines_skipped": 0
  },
  "status": {
    "usage_fetched_at": "...", "usage_from_cache": true,
    "stale": false,                  // refresh failed, cached data shown
    "usage_error": "...", "profile_error": "...", "local_error": "...",
    "auth_error": true,              // usage_error needs you to act: login expired or
                                     // rejected, 403, credentials missing/unreadable
    "credential_source": "/home/me/.claude/.credentials.json",
    "no_limit_data": false,
    "login_refreshed": true,         // --refresh-login renewed the login this run
    "login_refresh_error": "..."     // why --refresh-login could not renew it
  }
}
```

## How it works

**Remaining usage** comes from `GET https://api.anthropic.com/api/oauth/usage`,
the endpoint behind Claude Code's `/usage` command. **Plan info** comes from
`GET /api/oauth/profile`. Both are called with your Claude Code OAuth access
token (`Authorization: Bearer …`, `anthropic-beta: oauth-2025-04-20`).

**Credentials** are looked up in this order:

1. `--token`
2. `$CLAUDE_CODE_OAUTH_TOKEN`
3. macOS only: the keychain item Claude Code uses for the config dir, read
   with `security find-generic-password -s <item> -w`. For the default
   `~/.claude` that is `Claude Code-credentials`. For a config dir set with
   `--config-dir` or `$CLAUDE_CONFIG_DIR` it is
   `Claude Code-credentials-<first 8 hex digits of sha256(dir)>`, as Claude
   Code names it; the default item is never used for another dir, so a second
   account never shows the first account's usage.
4. `<config dir>/.credentials.json` (`$CLAUDE_CONFIG_DIR`, `--config-dir`, or
   `~/.claude`)

The token is never printed, logged or cached.

**Tokens are never refreshed.** Refreshing an OAuth token rotates the refresh
token, which would log Claude Code out, so this tool only reads credentials.
If the stored token has expired, the request is still attempted, because some
setups inject credentials through a proxy. On a 401 the tool says
`login expired · run claude`; any Claude Code session refreshes the login
(for a `--token` or `CLAUDE_CODE_OAUTH_TOKEN` token it says
`token rejected (401)` instead). A 403 is reported as `access denied (403)`
(missing scope, or a proxy/firewall denial), and a proxy that refuses the
connection as `proxy refused (…)`. If the credentials are missing or
unreadable, the last cached data is still shown.

Retrying does not fix these errors (401, 403, missing or unreadable
credentials), so they are not shown like a passing failure. Instead of the
dim stale note, a red line comes first. It leads with the message and ends
with the age of the data below it, for example
`login expired · run claude · data 6d old`, so a narrow widget cuts the age,
not the action. Without cached data, the message stands on its own line.

**`--refresh-login`** (opt-in) lets Claude Code refresh the login for you.
When a request with the stored token gets a 401, the tool runs this once,
in a new empty temporary directory that is removed afterwards:

    claude -p 'Reply with just: OK' --model haiku --tools '' \
      --no-session-persistence --strict-mcp-config --disable-slash-commands \
      --settings '{"disableAllHooks":true}'

Claude Code refreshes its expired token before sending that request and
stores the new one. The tool then reads the credentials again and retries
with it. The tool still never touches the token itself. This costs one tiny
Haiku request, about once per expired login, and takes a few seconds. No
transcript is saved, so local stats don't count it. `claude` must be in
`PATH`. `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and
`CLAUDE_CODE_OAUTH_TOKEN` are removed from its environment, so it uses the
stored login. `CLAUDE_CONFIG_DIR` is set only for an explicit config dir.
Tokens from `--token` or `CLAUDE_CODE_OAUTH_TOKEN` are never refreshed.
The usage and plan requests share one attempt. Concurrent widgets take
turns through a lock in the cache dir, and a later one uses the token the
first one obtained. If the refresh fails (`claude` missing, timed out after
60 s, or still logged out), the error stays `login expired · run claude`
and no new attempt is made for 30 minutes. The reason is in
`login_refresh_error` in the JSON output.

**Caching.** Responses are cached in `$XDG_CACHE_HOME/claude-usage` (or
`~/.cache/claude-usage`). Files there are mode 0600 and the directory is 0700.
Usage data is cached for 120 s and plan data for 1 h. When a refresh fails
(network error, timeout, 429 or 5xx), the last good data is shown with a dim
`stale · fetched 12m ago · <reason>` line. After a failure, the tool waits 30 s
(or as long as `Retry-After` asks) before trying again, so several widgets
don't hammer a rate-limited endpoint. Refreshes take a lock file next to the
cache entry, so when several widgets refresh at once only one request is
made and the others read its result. If there is no cached data at all, a
one-line error is shown and the local stats are still rendered. Requests time
out after 10 s and send `User-Agent: claude-usage/<version>`. A non-default
`CLAUDE_CONFIG_DIR`, or an explicitly passed token, gets its own cache files.

**Local stats** come from Claude Code transcripts in `~/.claude/projects`
and `~/.config/claude/projects`, including subagent transcripts. When a
config dir is set explicitly (`--config-dir` or `$CLAUDE_CONFIG_DIR`), only
`<config dir>/projects` is read, since that dir may belong to a different
account. Symlinked transcript files and project directories are followed.
The weekly period is the subscription's current window while that window is
still running, otherwise the last 7 days. Each assistant
API response appears on several lines, one per content block, so responses
are de-duplicated by `message.id` + `requestId`, across files as well.
`<synthetic>` messages and malformed lines are skipped, lines of any length
are handled, files last modified before the oldest window needed are not
opened, and files are parsed in parallel.

## Caveats

* **The usage and profile endpoints are undocumented** and may change or
  disappear without notice. Parsing is deliberately tolerant: nulls, missing
  fields, unexpected types and unknown limit kinds are handled. A big change
  in the response format could still leave the tool with nothing to show.
* Polling the endpoint too often can get you rate-limited (HTTP 429). Keep
  `refreshInterval` at a minute or more.
* Pace and run-out projections assume the average rate since the window
  started continues. Treat them as a hint, not a forecast.
* Local stats only cover Claude Code on this machine. Usage from
  claude.ai, the desktop and mobile apps, or other machines counts towards
  your limits but is not in these numbers. Token counts are raw API token
  counts and are not the same thing as the percentages in your quota.
* Extra-usage amounts from the older `extra_usage` object are assumed to be in
  minor units (cents) unless the response gives `decimal_places`. The newer
  `spend` object, which states its exponent, is preferred when present.
* Transcripts are parsed again on every run (no incremental state). That
  takes milliseconds for typical use but grows with the size of the
  transcripts modified in the last week.

## Development

```sh
gofmt -l . && go vet ./... && go test ./...
go test ./internal/render -update   # regenerate golden files after layout changes
```

Layout:

```
cmd/claude-usage/     flags, orchestration, exit codes
internal/api/         HTTP client and tolerant parsing of usage/profile responses
internal/creds/       read-only credential lookup (flag, env, keychain, file)
internal/cache/       0600 on-disk response cache with stale fallback and back-off
internal/relogin/     --refresh-login: runs `claude -p` so Claude Code refreshes its login
internal/localstats/  transcript scanning, de-duplication and aggregation
internal/report/      normalized report model: labels, severity, pace, plan, spend
internal/render/      width-aware ANSI/plain renderer and JSON output
testdata/             recorded API responses (personal data scrubbed)
```
