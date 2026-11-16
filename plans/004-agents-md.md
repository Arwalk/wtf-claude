# Plan 004: Add an AGENTS.md that records the repo's invariants for coding agents

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `ls AGENTS.md CLAUDE.md 2>&1` → both must be
> "No such file or directory". If either exists, STOP: someone already wrote
> one, and this plan would overwrite it. Then check that the facts quoted in
> "Current state" still hold, using the verification commands in Step 1.

## Status

- **Priority**: P3
- **Effort**: S
- **Risk**: LOW (documentation only)
- **Depends on**: best run after 001-003, so the file describes the final
  state. It is not blocked by them.
- **Category**: dx / docs
- **Planned at**: no git repo, 2026-11-16

## Why this matters

`claude-usage` is a small Go CLI that coding agents maintain (the repo carries
agent skills under `.agents/skills/`). Its most important rules appear only in
README prose and in code comments. The token must never be printed or
refreshed. There must be no third-party dependencies. Personal details must
never be parsed. The JSON output is a documented contract. Golden files are
regenerated with a flag. An agent that misses one of these can do real damage:
refreshing the OAuth token logs the user out of Claude Code. A short
`AGENTS.md` (plus a `CLAUDE.md` that imports it, so Claude Code loads it
automatically) puts these rules where agents look first.

## Current state

Facts the file must state. Each one was verified against the code when this
plan was written, and Step 1 re-checks them.

- Module `claude-usage`, Go 1.22, no `require` block (`go.mod`). README line
  52: "Requires Go 1.22 or newer. There are no third-party dependencies."
- Dev gate (README lines 314-317):
  ```sh
  gofmt -l . && go vet ./... && go test ./...
  go test ./internal/render -update   # regenerate golden files after layout changes
  ```
- Layout (README lines 321-330):
  ```
  cmd/claude-usage/     flags, orchestration, exit codes
  internal/api/         HTTP client and tolerant parsing of usage/profile responses
  internal/creds/       read-only credential lookup (flag, env, keychain, file)
  internal/cache/       0600 on-disk response cache with stale fallback and back-off
  internal/localstats/  transcript scanning, de-duplication and aggregation
  internal/report/      normalized report model: labels, severity, pace, plan, spend
  internal/render/      width-aware ANSI/plain renderer and JSON output
  testdata/             recorded API responses (personal data scrubbed)
  ```
- Token rules: `internal/creds/creds.go` lines 1-4 ("It only ever reads
  credentials: it never refreshes tokens or writes the credentials file,
  because refreshing rotates the refresh token and would log Claude Code
  out."). README line 252: "The token is never printed, logged or cached."
  `cmd/claude-usage/main_test.go` `TestEndToEnd` asserts the output never
  contains the test token.
- Personal data: `internal/api/profile.go` lines 8-9 ("Personal details
  (email, full name) are deliberately not parsed.").
- Tolerant parsing: `internal/api/usage.go` lines 98-100 ("Only a body that is
  not a JSON object is an error; every individual field is optional and
  tolerant of nulls, type surprises and unknown values."). This uses the
  `flex*` types in `internal/api/flex.go`.
- JSON contract: `internal/report/report.go` lines 31-32 ("Its JSON form is
  the --format json contract documented in README.md."). README section
  "### JSON output" (starts around line 174).
- Width invariant: `internal/render/render.go` `MinWidth = 12`. README:
  "Lines are never wider than the requested width (the minimum is 12)." Tests
  `TestTinyWidthNeverOverflows` and `TestGoldenWidths` enforce it.
- Error messages put the reason first: `internal/creds/creds.go` `FileError`
  ("Its message puts the reason before the path so it survives truncation in
  narrow widgets").
- Platform code uses build-tagged pairs: `internal/cache/lock_flock.go` /
  `internal/cache/lock_other.go`.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Fact checks | see Step 1 | each grep prints ≥1 line |
| Tests (sanity) | `go test ./...` | all `ok` (docs-only change) |

## Scope

**In scope** (create only):
- `AGENTS.md`
- `CLAUDE.md`

**Out of scope**:
- `README.md`. Do not move or duplicate README content. `AGENTS.md` links to
  it.
- Any `.go` file.
- `.agents/`, `.claude/`, `skills-lock.json`. These are tooling, not project
  docs.

## Git workflow

- If the directory is a git repository when you execute, create branch
  `advisor/004-agents-md` and commit with message
  `Add AGENTS.md with repo invariants`. Otherwise edit in place.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Re-verify the facts

Run each command. Every one must print at least one line:

```sh
grep -n "There are no third-party dependencies" README.md
grep -n "never refreshes tokens" internal/creds/creds.go
grep -n "never printed, logged or cached" README.md
grep -n "deliberately not parsed" internal/api/profile.go
grep -n "contract documented in README.md" internal/report/report.go
grep -n "MinWidth = 12" internal/render/render.go
grep -n "reason before the path" internal/creds/creds.go
grep -n "\-update" README.md
```

Also run `grep -n "func stripControls" internal/render/text.go`. If it prints
a line, plan 003 has landed: include the optional bullet marked **[003]** in
Step 2. If it prints nothing, leave that bullet out. Likewise, run
`grep -n "type ScanError" internal/localstats/localstats.go`. If it prints a
line, include the bullet marked **[002]**; otherwise leave it out.

**Verify**: all eight greps print a line. If any prints nothing, STOP.

### Step 2: Write `AGENTS.md`

Create `AGENTS.md` with exactly this content. Drop the **[002]**/**[003]**
bullets according to Step 1, and remove the bracketed markers from any bullet
you keep:

```markdown
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
  against a copy of `testdata/` taken before regenerating). Never regenerate goldens
  to make an unexplained failure go away.
- Platform-specific code: also run
  `for os in darwin windows freebsd; do GOOS=$os go vet ./...; done`.

## Hard rules

- **Never refresh, write or log the OAuth token.** Refreshing rotates the
  refresh token and logs the user out of Claude Code. `internal/creds` is
  read-only; the token must never appear in output, errors, cache files or
  cache file names (cache keys use a short hash).
- **No third-party dependencies.** `go.mod` has no `require` block; use the
  standard library (`syscall` behind build tags is fine).
- **No personal data.** Email and names from the profile endpoint are not
  parsed. Fixtures in `testdata/` must be scrubbed before committing.
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
- [003] **All text output goes through `render`'s `line`/`seg` types**, which
  strip terminal control characters (`stripControls` in
  `internal/render/text.go`). Never write API or transcript strings to stdout
  any other way. JSON output keeps raw strings.

## Conventions

- Short user-facing errors that lead with the actionable part and put paths
  last: widgets truncate long lines. See `creds.FileError`.
  [002] `localstats.ScanError` follows the same pattern.
- Platform code comes in build-tagged pairs with a harmless fallback, e.g.
  `internal/cache/lock_flock.go` / `lock_other.go`.
- Exit codes: 0 when anything useful was printed; inside wtfutil
  (`WTF_WIDGET_WIDTH` set) errors exit 0 because the message is the widget's
  content. See README "Usage".
- Tests isolate themselves with `t.TempDir()`, `t.Setenv("HOME", …)` and
  `httptest` servers (see `setup` in `cmd/claude-usage/main_test.go`); new
  tests must not depend on the real `~/.claude` or the network.

## Layout

    cmd/claude-usage/     flags, orchestration, exit codes
    internal/api/         HTTP client and tolerant parsing of usage/profile responses
    internal/creds/       read-only credential lookup (flag, env, keychain, file)
    internal/cache/       0600 on-disk response cache with stale fallback and back-off
    internal/localstats/  transcript scanning, de-duplication and aggregation
    internal/report/      normalized report model: labels, severity, pace, plan, spend
    internal/render/      width-aware ANSI/plain renderer and JSON output
    testdata/             recorded API responses (personal data scrubbed)
    plans/                improvement plans; update plans/README.md status rows
```

**Verify**: `grep -c "Never refresh, write or log the OAuth token" AGENTS.md`
→ `1`, and `grep -c "\[00[23]\]" AGENTS.md` → `0` (no leftover markers).

### Step 3: Write `CLAUDE.md`

Create `CLAUDE.md` with exactly:

```markdown
@AGENTS.md
```

(Claude Code reads `CLAUDE.md` and expands `@path` imports. Other agents read
`AGENTS.md` directly.)

**Verify**: `cat CLAUDE.md` → `@AGENTS.md`

### Step 4: Sanity gate

**Verify**: `go test ./...` → all `ok` (nothing should change; this confirms
no stray file broke the build), and `ls *.md` lists `AGENTS.md`, `CLAUDE.md`
and `README.md`.

## Test plan

Documentation only; no new tests. The Step 1 greps are the check that every
claim in `AGENTS.md` is true of the code.

## Done criteria

- [ ] `AGENTS.md` and `CLAUDE.md` exist at the repo root
- [ ] All eight Step 1 greps print a line
- [ ] `grep -c "\[00[23]\]" AGENTS.md` → `0`
- [ ] `go test ./...` all `ok`
- [ ] No other file modified
- [ ] `plans/README.md` status row for 004 updated

## STOP conditions

Stop and report back (do not improvise) if:

- `AGENTS.md` or `CLAUDE.md` already exists.
- Any Step 1 grep prints nothing. A fact in the file would be false.
- You are tempted to add a rule not listed here. Report it as a suggestion
  instead. This file must only state facts the code enforces or the README
  documents.

## Maintenance notes

- When a rule changes (e.g. a dependency is ever allowed), update `AGENTS.md`
  in the same change. A stale rules file is worse than none.
- Keep it short. Behaviour belongs in `README.md`; `AGENTS.md` is for rules
  that, when broken, hurt users or break the build.
