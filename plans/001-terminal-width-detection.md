# Plan 001: Size output to the real terminal when run in a plain terminal

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: this directory was not a git repository when
> the plan was written, so drift is checked by content hash. Run:
> `sha256sum cmd/claude-usage/main.go cmd/claude-usage/main_test.go README.md | cut -c1-16`
> Expected (as planned):
> ```
> 94cd4f1df6df1543  (cmd/claude-usage/main.go)
> 78937c84ffe474fa  (cmd/claude-usage/main_test.go)
> 03c05b4a02ec8afa  (README.md)
> ```
> If a hash differs, compare every "Current state" excerpt below against the
> live file. If the excerpts still match verbatim, proceed. If any excerpt does
> not match, STOP. If the repo is now under git, also run
> `git log --oneline -5 -- cmd/claude-usage README.md` and note what changed.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug (UX)
- **Planned at**: no git repo (content hashes above), 2026-11-16

## Why this matters

`claude-usage` is a Go CLI that prints Claude subscription usage with bars sized
to a width in terminal cells. Without `--width` or the wtfutil env vars, it reads
`$COLUMNS`, then falls back to 60. Interactive shells do not export `COLUMNS` to
child processes (verified: `bash -c 'printenv COLUMNS'` prints nothing). So in
an ordinary terminal the tool always renders 60 cells wide. In a terminal
narrower than 60 columns, every line wraps and the layout breaks. This plan
asks the kernel for the terminal size (ioctl `TIOCGWINSZ`) when stdout is a
TTY and uses it, capped at 80 so wide terminals don't get very long bars. It
adds no third-party dependencies: the repo has none, and it must stay that way.

## Current state

Files involved:

- `cmd/claude-usage/main.go`: flags, environment handling, orchestration.
  - Lines 150-156, the `env` struct that abstracts the process environment for
    tests:
    ```go
    // env abstracts the process environment for testing.
    type env struct {
    	getenv func(string) string
    	isTTY  bool
    	now    func() time.Time
    	loc    *time.Location
    }
    ```
  - Lines 158-171, `outputWidth`:
    ```go
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
    ```
  - Line 81, the `--width` flag help:
    ```go
    	fs.IntVar(&c.width, "width", 0, "output width in cells (default: $WTF_WIDGET_WIDTH, $COLUMNS, or 60)")
    ```
  - Lines 224-226 in `run`, where TTY detection already happens:
    ```go
    	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
    		e.isTTY = true
    	}
    ```
- `cmd/claude-usage/main_test.go`: tests. `TestOutputWidth` (lines 23-40)
  uses the `fakeEnv` helper (lines 19-21):
  ```go
  func fakeEnv(m map[string]string, tty bool) env {
  	return env{getenv: func(k string) string { return m[k] }, isTTY: tty}
  }
  ```
- `README.md` lines 73-74, in the Usage block:
  ```
    --width N                  width in cells (default: $WTF_WIDGET_WIDTH,
                               then $COLUMNS, then 60)
  ```

Conventions to match:

- Platform-specific code uses build-tagged file pairs. The exemplar is
  `internal/cache/lock_flock.go` (first line
  `//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly`)
  and `internal/cache/lock_other.go` (first line
  `//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)`),
  which returns a harmless fallback. Use exactly these two tag expressions.
- Standard library only. `syscall` + `unsafe` are fine. Do NOT add
  `golang.org/x/term` or `golang.org/x/sys`; `go.mod` must stay dependency-free.
- Doc comments on every function; short, lowercase-first error strings; `gofmt`.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Format check | `gofmt -l .` | no output |
| Vet | `go vet ./...` | exit 0, no output |
| Tests | `go test ./...` | all `ok` |
| Cross-vet | `for os in darwin freebsd openbsd netbsd dragonfly windows; do GOOS=$os GOARCH=amd64 go vet ./... \|\| echo FAIL $os; done` | no `FAIL` lines |
| Build | `go build -o /tmp/cu ./cmd/claude-usage` | exit 0 |

## Scope

**In scope** (the only files you should modify or create):
- `cmd/claude-usage/termsize_unix.go` (create)
- `cmd/claude-usage/termsize_other.go` (create)
- `cmd/claude-usage/main.go`
- `cmd/claude-usage/main_test.go`
- `README.md` (only the `--width` lines in the Usage block)

**Out of scope** (do NOT touch):
- `internal/render/**`: layout logic and golden files. Width handling there is
  correct for any width it is given; this plan only changes which width it is
  given.
- The precedence of `--width`, `WTF_WIDGET_WIDTH` and `COLUMNS`. They must keep
  winning over the detected terminal size, in that order, because wtfutil
  widgets depend on `WTF_WIDGET_WIDTH`.
- Height (`outputHeight`). Do not auto-detect terminal height. Dropping lines
  to fit a terminal's height would surprise terminal users.
- Windows console size detection. Leave it as the fallback (0).

## Git workflow

- If the directory is a git repository when you execute, create branch
  `advisor/001-terminal-width` and commit once at the end with message
  `Detect terminal width when stdout is a TTY`. Otherwise edit in place.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add the platform files

Create `cmd/claude-usage/termsize_unix.go`:

```go
//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth returns the column count of the terminal f is attached to,
// or 0 if f is not a terminal or the size is unknown.
func terminalWidth(f *os.File) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
```

Create `cmd/claude-usage/termsize_other.go`:

```go
//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "os"

// terminalWidth is not implemented on this platform; callers fall back to
// the default width.
func terminalWidth(*os.File) int { return 0 }
```

**Verify**: `go vet ./... && for os in darwin freebsd openbsd netbsd dragonfly windows; do GOOS=$os GOARCH=amd64 go vet ./cmd/... || echo FAIL $os; done`
→ no output at all, and in particular no `FAIL` lines. (`terminalWidth` is
not called yet. That is fine: `go vet` does not report unused functions.)

### Step 2: Thread the terminal width through `env` and `outputWidth`

In `cmd/claude-usage/main.go`:

1. Add a `termWidth` field to `env`. The whole struct then reads as follows,
   gofmt-aligned. The longer field name re-aligns the other fields too, which
   is expected:
   ```go
   // env abstracts the process environment for testing.
   type env struct {
   	getenv func(string) string
   	isTTY  bool
   	// termWidth is the terminal's column count when stdout is a TTY (0 = unknown).
   	termWidth int
   	now       func() time.Time
   	loc       *time.Location
   }
   ```
   After editing, run `gofmt -w cmd/claude-usage/main.go`. gofmt owns the
   alignment, so accept whatever it produces.
2. Add a constant above `outputWidth`:
   ```go
   // maxAutoWidth caps a width taken from the terminal size, so a very wide
   // terminal doesn't stretch the bars across the whole screen. Explicit
   // widths (--width, WTF_WIDGET_WIDTH, COLUMNS) are not capped.
   const maxAutoWidth = 80
   ```
3. In `outputWidth`, insert this between the `COLUMNS` check and `return 60`:
   ```go
   	if e.termWidth > 0 {
   		return min(e.termWidth, maxAutoWidth)
   	}
   ```
4. In `run`, extend the existing TTY block (lines 224-226) to:
   ```go
   	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
   		e.isTTY = true
   		e.termWidth = terminalWidth(os.Stdout)
   	}
   ```
5. Update the `--width` help string (line 81) to:
   `"output width in cells (default: $WTF_WIDGET_WIDTH, $COLUMNS, the terminal width up to 80, or 60)"`

**Verify**: `gofmt -l . && go vet ./... && go test ./cmd/...` → no gofmt
output, vet clean, `ok  claude-usage/cmd/claude-usage`.

### Step 3: Extend the tests

In `cmd/claude-usage/main_test.go`, add a new test after `TestOutputWidth`. Do
not rewrite `TestOutputWidth`. Model it on `TestOutputWidth`'s style:

```go
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
```

**Verify**: `go test ./cmd/... -run 'OutputWidth|TerminalWidth' -v` → both new
tests and `TestOutputWidth` show `--- PASS`.

### Step 4: Update the README

In `README.md`, replace the two `--width` lines of the Usage block (lines
73-74) with:

```
  --width N                  width in cells (default: $WTF_WIDGET_WIDTH,
                             then $COLUMNS, then the terminal width up to
                             80, then 60)
```

Keep the alignment of the surrounding block: descriptions start at column 30.

**Verify**: `grep -n "terminal width up to" README.md` → exactly one match.

### Step 5: Full gate and manual smoke test

**Verify**:
- `gofmt -l .` → no output
- `go vet ./...` → exit 0
- `go test ./...` → all `ok`
- `go build -o /tmp/cu ./cmd/claude-usage && env -u COLUMNS -u WTF_WIDGET_WIDTH /tmp/cu --sections local --format plain | awk '{ if (length($0) > m) m = length($0) } END { print (m <= 60) ? "ok" : "too wide" }'`
  → `ok`. Stdout is a pipe here, so the width must still default to 60.
  (`awk length` counts bytes in some locales; the lines here are mostly
  ASCII, so this check is good enough.)

## Test plan

- `TestOutputWidthFromTerminal` (new): narrow terminal used as-is, wide
  terminal capped at 80, precedence of `COLUMNS` and `WTF_WIDGET_WIDTH` over the
  terminal, an unknown size falling back to 60, and `--width` beating
  everything.
- `TestTerminalWidthNotATTY` (new): the ioctl on a non-terminal returns 0
  without error or panic. Runs on every platform; on non-unix it exercises the
  stub.
- Existing `TestOutputWidth` must still pass unchanged. It proves the old
  behaviour when `termWidth` is 0.
- A real-TTY test is not possible without a pty dependency; that is
  intentionally not covered (see Maintenance notes).

## Done criteria

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` exits 0
- [ ] Cross-vet loop (Commands table) prints no `FAIL`
- [ ] `go test ./...` all `ok`, including `TestOutputWidthFromTerminal` and `TestTerminalWidthNotATTY`
- [ ] `grep -c "terminalWidth(os.Stdout)" cmd/claude-usage/main.go` → `1`
- [ ] `cat go.mod` still shows only `module claude-usage` and `go 1.22` (no `require`)
- [ ] Only the in-scope files are modified or created
- [ ] `plans/README.md` status row for 001 updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt does not match the live code.
- `syscall.TIOCGWINSZ` or `syscall.SYS_IOCTL` is undefined for any GOOS in
  the cross-vet loop. Report which one; do not narrow the build tags yourself.
- Any existing golden test in `internal/render` fails. This plan must not
  change rendering for an explicit width.
- Making it work seems to require a new module dependency.

## Maintenance notes

- `syscall.Syscall` on darwin goes through Go's libc trampoline. It compiles
  (verified by cross-vet when this plan was written), but no maintainer has
  run it on macOS yet. After landing, a maintainer on macOS should run
  `claude-usage --sections local` in a terminal narrower than 60 columns and
  confirm the lines don't wrap.
- OpenBSD has been restricting raw `syscall(2)` use. The ioctl compiles there
  but may fail at runtime. If it does, `terminalWidth` returns 0 and the old
  60-cell default applies, so the failure is harmless.
- The 80-cell cap (`maxAutoWidth`) is a deliberate product choice. Bars wider
  than about 50 cells add no information. If users ask for full-width output
  they can still pass `--width` or export `COLUMNS`.
- Windows console size detection (`GetConsoleScreenBufferInfo`) is deferred.
  The stub keeps the old 60-cell behaviour there.
- Reviewers: check that the order in `outputWidth` is flag → widget → COLUMNS →
  terminal → 60, and that the terminal is only queried when stdout is a
  character device.
