# AGENTS.md

`claude-usage` is a Go CLI that shows remaining Claude subscription usage and
local Claude Code token stats, mainly inside a wtfutil `cmdrunner` widget.
User-facing behaviour is documented in `README.md`; read it before changing
behaviour.

## Verify every change

    gofmt -l . && go vet ./... && go test ./...

- `gofmt -l .` must print nothing.
- Layout changes: `go test ./internal/render -update`, then read the diff of
  `internal/render/testdata/*.golden` line by line (with `git diff`, or
  against a copy of `testdata/` taken before regenerating). Never regenerate
  goldens to make an unexplained failure go away.
- Platform-specific code: also run
  `for os in darwin windows freebsd; do GOOS=$os go vet ./...; done`.

## Hard rules

- **Never refresh, write or log the OAuth token.** Refreshing rotates the
  refresh token and logs the user out of Claude Code. `internal/creds` is
  read-only; the token must never appear in output, errors, cache files or
  cache file names (cache keys use a short hash). The only way a login gets
  refreshed is the opt-in `--refresh-login`. It runs `claude -p`
  (`internal/relogin`), so Claude Code refreshes its own token, and then
  reads the credentials again.
- **No third-party dependencies.** `go.mod` has no `require` block; use the
  standard library (`syscall` behind build tags is fine).
- **The token goes only to the configured server over a safe channel.**
  `--base-url` must be https, or http to loopback (`checkBaseURL`), and
  `api.Client` never follows redirects. Errors about the command line are
  redacted and never quote positional arguments: one of them may be the token.
- **No personal data.** Email and names from the profile endpoint are not
  parsed, and `api.TrimProfile` drops them before anything is cached.
  Fixtures in `testdata/` must be scrubbed before committing, including ids
  and account timestamps.
- **This repository is public.** Don't write unfixed security problems,
  personal data, machine details or ids of unpublished commits into `plans/`,
  commit messages or anything else that is tracked. See `SECURITY.md`.
- **Tolerant parsing.** The usage/profile endpoints are undocumented. Only a
  non-object body is an error; every field may be null, missing or the wrong
  type (use the `flex*` types in `internal/api/flex.go`).
- **The JSON output is a contract.** `--format json` is documented in
  README "JSON output" and mirrors the JSON tags in `internal/report` (and
  `internal/localstats` for `local`). Add fields freely; never rename or
  remove one; update the README section in the same change.
- **Lines never exceed the width** (minimum 12 cells), and with a height the
  output never exceeds it. Keep `TestTinyWidthNeverOverflows` and
  `TestHeightFitting` passing.
- **All text output goes through `render`'s `line`/`seg` types**, which
  strip terminal control characters (`stripControls` in
  `internal/render/text.go`). Never write API or transcript strings to stdout
  any other way. JSON output keeps raw strings.

## Conventions

- Short user-facing errors that lead with the actionable part and put paths
  last: widgets truncate long lines. See `creds.FileError`.
  `localstats.ScanError` follows the same pattern.
- Platform code comes in build-tagged pairs with a harmless fallback, e.g.
  `internal/cache/lock_flock.go` / `lock_other.go`.
- Exit codes: 0 when anything useful was printed, 1 when only an error could
  be shown, 2 for invalid flags. Inside wtfutil (`WTF_WIDGET_WIDTH` set) the
  "only an error" case exits 0 instead, because the message is the widget's
  content; invalid flags still exit 2 but are also printed to stdout, since
  cmdrunner discards stderr. See README "Usage".
- `run()` uses the real clock, so no test may depend on a date in a fixture
  still being in the future (see `currentFixture`).
- Tests isolate themselves with `t.TempDir()`, `t.Setenv("HOME", …)` and
  `httptest` servers (see `setup` in `cmd/claude-usage/main_test.go`, which
  also hides the macOS keychain); new
  tests must not depend on the real `~/.claude` or the network.

## Layout

    cmd/claude-usage/     flags, orchestration, exit codes
    internal/api/         HTTP client and tolerant parsing of usage/profile responses
    internal/creds/       read-only credential lookup (flag, env, keychain, file)
    internal/cache/       0600 on-disk response cache with stale fallback and back-off
    internal/relogin/     --refresh-login: runs `claude -p` so Claude Code refreshes its login
    internal/localstats/  transcript scanning, de-duplication and aggregation
    internal/report/      normalized report model: labels, severity, pace, plan, spend
    internal/render/      width-aware ANSI/plain renderer and JSON output
    testdata/             recorded API responses (personal data scrubbed)
    .github/workflows/    CI: the checks above on Linux, macOS and Windows
    plans/                improvement plans; update plans/README.md status rows
