# Plan 003: Strip terminal control characters from output and make string cuts UTF-8 safe

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: this directory was not a git repository when
> the plan was written, so drift is checked by content hash. Run:
> `sha256sum internal/render/text.go internal/render/render_test.go internal/report/report.go internal/report/report_test.go cmd/claude-usage/main.go cmd/claude-usage/main_test.go internal/api/client.go internal/api/api_test.go | cut -c1-16`
> Expected (as planned):
> ```
> 603cc6ef521a2922  (internal/render/text.go)
> 6e873507aa358d2a  (internal/render/render_test.go)
> 46ed8defde05733b  (internal/report/report.go)
> e7a7f549312a080a  (internal/report/report_test.go)
> 94cd4f1df6df1543  (cmd/claude-usage/main.go)
> 78937c84ffe474fa  (cmd/claude-usage/main_test.go)
> ceb0b2fd8af1af11  (internal/api/client.go)
> 9c520e1adc72e647  (internal/api/api_test.go)
> ```
> Plans 001 and 002 legitimately change `main.go`, `main_test.go` and
> `render_test.go`, so those hashes will differ if they have landed. Line
> numbers in those files may also have shifted by a few lines. For every
> differing hash, find each "Current state" excerpt by content. If every
> excerpt still exists verbatim, proceed. If one does not, STOP.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none. If 001/002 are also being executed, run them first;
  they touch the same test files.
- **Category**: security (hardening) + bug
- **Planned at**: no git repo (content hashes above), 2026-11-16

## Why this matters

`claude-usage` prints text that comes from outside the program straight to
the terminal (or into a wtfutil widget, which interprets ANSI escapes). That
text includes limit kinds, model and surface names, breakdown names, plan
labels and currency codes from the (undocumented) usage API, plus model names
read from local Claude Code transcript files. None of it is filtered. An ESC
character in any of those strings would reach the terminal as a live escape
sequence: it could recolour or clear the screen, set the window title, or
hide text. A newline would add rows and break the height-fitting logic. The
width code also counts control characters as 0 cells, so the "lines never
exceed the width" guarantee can't be trusted for such text. Exploiting this
needs a hostile response (an intercepting proxy, or `--base-url` pointed
elsewhere) or a crafted transcript, so this is cheap hardening, not an
emergency.

The plan also fixes some byte-indexed string handling that mangles non-ASCII
text. Capitalising with `s[:1]` splits a multi-byte first letter, and cutting
error messages at a byte count can split a UTF-8 character. Example: a
credentials path under `/home/José/` in an error message.

## Current state

Files involved:

- `internal/render/text.go`: styled segments and their final rendering.
  - Lines 91-109, `line.render`, the single place every output segment is
    written:
    ```go
    // render writes the line using ANSI colours when color is true.
    func (l line) render(color bool) string {
    	var b strings.Builder
    	for _, s := range l {
    		if s.text == "" {
    			continue
    		}
    		if color && s.st != plain {
    			b.WriteString("\x1b[")
    			b.WriteString(s.st.sgr())
    			b.WriteString("m")
    			b.WriteString(s.text)
    			b.WriteString("\x1b[0m")
    		} else {
    			b.WriteString(s.text)
    		}
    	}
    	return strings.TrimRight(b.String(), " ")
    }
    ```
  - Lines 120-123, `runeWidth` treats control characters as zero-width:
    ```go
    func runeWidth(r rune) int {
    	switch {
    	case r == 0, r < 32, r >= 0x7f && r < 0xa0:
    		return 0
    ```
  - `render.Lines` (`internal/render/render.go:100-104`) calls
    `l.truncate(opt.Width).render(opt.Color)` for every line, so `render` sees
    all text output.
- `internal/report/report.go`: builds labels from API strings.
  - Lines 291-298, `humanize`:
    ```go
    // humanize turns "weekly_opus_thing" into "Weekly opus thing".
    func humanize(s string) string {
    	s = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(s))
    	if s == "" {
    		return ""
    	}
    	return strings.ToUpper(s[:1]) + s[1:]
    }
    ```
  - Line 307 in `titleWord`: `words[i] = strings.ToUpper(w[:1]) + w[1:]`
  - Line 544 in `TierLabel`: `words[i] = strings.ToUpper(w[:1]) + w[1:]`
- `cmd/claude-usage/main.go`, near the end, `shortErr`:
  ```go
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
  ```
- `internal/api/client.go` lines 169-171, the end of `Describe`:
  ```go
  	if len(msg) > 60 {
  		msg = msg[:57] + "..."
  	}
  ```

Conventions to match:

- Standard library only (`go.mod` has no `require` and must keep none).
- Every function has a doc comment explaining *why*, as in `text.go`.
- The JSON output (`render.JSON`) must stay a faithful copy of the data. Do
  NOT sanitize the report model or JSON. `encoding/json` already escapes C0
  control characters, and JSON consumers are responsible for their own
  display. Only the text renderer filters.
- Tests: `internal/render/render_test.go` builds reports with `fixtureReport(t)`
  and renders with `renderString(t, r, Options{Width: 60, Loc: cet})`. See
  `TestPlainHasNoEscapes` (around line 126) for the shape.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Format check | `gofmt -l .` | no output |
| Vet | `go vet ./...` | exit 0 |
| Tests | `go test ./...` | all `ok` |
| Single package | `go test ./internal/render/ -run '<name>' -v` | named tests PASS |

## Scope

**In scope** (the only files you should modify):
- `internal/render/text.go`
- `internal/render/render_test.go`
- `internal/report/report.go`
- `internal/report/report_test.go`
- `cmd/claude-usage/main.go` (only `shortErr` plus a helper and an import)
- `cmd/claude-usage/main_test.go`
- `internal/api/client.go` (only the truncation in `Describe` plus a helper and an import)
- `internal/api/api_test.go`

**Out of scope** (do NOT touch):
- `internal/render/testdata/*.golden`. The fixture is plain ASCII, so the
  output must be byte-identical. Never run with `-update` for this plan.
- `render.JSON` and the `report` struct fields (see conventions).
- `runeWidth`'s table. Changing the width rules is a separate concern.
- wtfutil/tview markup escaping (`[red]`-style tags). That is a different
  problem, and escaping it would corrupt plain-terminal output (see
  Maintenance notes).
- `internal/creds/**`. Credential-file error messages are built there but only
  truncated in `shortErr`.

## Git workflow

- If the directory is a git repository when you execute, create branch
  `advisor/003-text-hygiene` and commit once at the end with message
  `Strip control characters from text output; UTF-8-safe truncation`.
  Otherwise edit in place.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Filter unsafe characters in `line.render`

In `internal/render/text.go`, add below `render`:

```go
// stripControls removes characters a terminal would act on instead of
// printing: C0/C1 control codes (ESC starts escape sequences, a newline would
// add a row) and Unicode bidirectional overrides, which can reorder what is
// displayed. Labels come from an undocumented API and from transcript files,
// so every segment passes through here before it is written.
func stripControls(s string) string {
	if strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return -1
		}
		return r
	}, s)
}

// unsafeRune reports whether r is a control or bidi-override character.
func unsafeRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r < 0xa0) ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
```

Then, in `render`, sanitize each segment's text once at the top of the loop
body and use that variable everywhere `s.text` is used below it:

```go
	for _, s := range l {
		text := stripControls(s.text)
		if text == "" {
			continue
		}
		if color && s.st != plain {
			b.WriteString("\x1b[")
			b.WriteString(s.st.sgr())
			b.WriteString("m")
			b.WriteString(text)
			b.WriteString("\x1b[0m")
		} else {
			b.WriteString(text)
		}
	}
```

Our own SGR sequences are written directly, not through `s.text`, so colours
still work.

**Verify**: `go test ./internal/render/` → `ok`. All golden tests pass
unchanged.

### Step 2: Render tests

Append to `internal/render/render_test.go`:

```go
func TestStripControls(t *testing.T) {
	cases := map[string]string{
		"plain":               "plain",
		"Weekly · Fable ▸ 日本": "Weekly · Fable ▸ 日本",
		"a\x1b[31mb":          "a[31mb",
		"t\x1b]0;x\x07":       "t]0;x",
		"line\nbreak\ttab":    "linebreaktab",
		"c1\u009bx":           "c1x",
		"bidi\u202eevil\u2066": "bidievil",
	}
	for in, want := range cases {
		if got := stripControls(in); got != want {
			t.Errorf("stripControls(%q) = %q, want %q", in, got, want)
		}
	}
}

// Text from the API and transcripts can never inject escapes or extra rows.
func TestUntrustedTextIsSanitized(t *testing.T) {
	r := fixtureReport(t)
	r.Plan.Display = "Ultra\nplan"
	r.Limits[0].Label = "Weekly \x1b[2J\x1b]0;pwned\x07 all"
	r.Limits[0].ShortLabel = "All\x1b[31m"
	r.Breakdown.Rows[0].Name = "Claude\u202eCode"
	r.Local.Week.Models[0].Model = "claude-opus\x1b[5m"
	clean := renderString(t, fixtureReport(t), Options{Width: 60, Loc: cet})
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Errorf("control characters leaked:\n%q", got)
	}
	if strings.Count(got, "\n") != strings.Count(clean, "\n") {
		t.Errorf("a newline in a label added a row:\n%s", got)
	}
	for i, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if cw := cellWidth(l); cw > 60 {
			t.Errorf("line %d is %d cells: %q", i+1, cw, l)
		}
	}
}
```

The test file's `gofmt` alignment of the map literal may differ from the
above; run `gofmt -w internal/render/render_test.go` after pasting.

**Verify**: `go test ./internal/render/ -run 'StripControls|UntrustedText' -v` → both `PASS`.

### Step 3: Rune-aware capitalisation in `report`

In `internal/report/report.go`:

1. Add `"unicode"` and `"unicode/utf8"` to the imports.
2. Add below `titleWord`:
   ```go
   // upperFirst upper-cases the first character of s without splitting a
   // multi-byte UTF-8 character (s[:1] would).
   func upperFirst(s string) string {
   	r, n := utf8.DecodeRuneInString(s)
   	if r == utf8.RuneError {
   		return s // empty or invalid: leave as is
   	}
   	return string(unicode.ToUpper(r)) + s[n:]
   }
   ```
3. Replace the three byte-indexed capitalisations:
   - in `humanize`: `return strings.ToUpper(s[:1]) + s[1:]` → `return upperFirst(s)`
   - in `titleWord`: `words[i] = strings.ToUpper(w[:1]) + w[1:]` → `words[i] = upperFirst(w)`
   - in `TierLabel`: `words[i] = strings.ToUpper(w[:1]) + w[1:]` → `words[i] = upperFirst(w)`

Append to `internal/report/report_test.go`:

```go
func TestLabelsNonASCII(t *testing.T) {
	cases := []struct{ got, want string }{
		{LimitLabel("éclair_kind", "", ""), "Éclair kind"},
		{LimitLabel("seven_day_ébène", "", ""), "Weekly · Ébène"},
		{ShortLabel("seven_day_ébène", "", ""), "Ébène"},
		{TierLabel("default_claude_über_5x"), "Über 5x"},
		{LimitLabel("monthly_moonbeam", "", ""), "Monthly moonbeam"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
```

**Verify**: `grep -c 'ToUpper(s\[:1\])\|ToUpper(w\[:1\])' internal/report/report.go`
→ `0`, and `go test ./internal/report/` → `ok`.

### Step 4: UTF-8-safe truncation of error messages

1. In `cmd/claude-usage/main.go`, add `"unicode/utf8"` to the imports and add
   below `shortErr`:
   ```go
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
   ```
   In `shortErr`, change `msg = msg[:117] + "..."` to
   `msg = cutAtRune(msg, 117) + "..."`.
2. In `internal/api/client.go`, add `"unicode/utf8"` to the imports and add
   the same `cutAtRune` function, with the same doc comment, below
   `isStatusText`. It is duplicated on purpose: `cmd` and `api` share no
   utility package, and five lines don't justify creating one. In `Describe`,
   change `msg = msg[:57] + "..."` to `msg = cutAtRune(msg, 57) + "..."`.
3. Append to `cmd/claude-usage/main_test.go` (add `"errors"` and
   `"unicode/utf8"` to its imports if missing):
   ```go
   func TestShortErrKeepsUTF8(t *testing.T) {
   	got := shortErr(errors.New(strings.Repeat("é", 100))) // 200 bytes
   	if !utf8.ValidString(got) || len(got) > 120 || !strings.HasSuffix(got, "...") {
   		t.Errorf("shortErr = %q (%d bytes)", got, len(got))
   	}
   }
   ```
4. Append to `internal/api/api_test.go` (add `"errors"` and `"unicode/utf8"`
   to its imports if missing):
   ```go
   func TestDescribeKeepsUTF8(t *testing.T) {
   	got := Describe(errors.New(strings.Repeat("é", 50))) // 100 bytes
   	if !utf8.ValidString(got) || len(got) > 60 || !strings.HasSuffix(got, "...") {
   		t.Errorf("Describe = %q (%d bytes)", got, len(got))
   	}
   }
   ```

**Verify**: `grep -n 'msg\[:117\]\|msg\[:57\]' cmd/claude-usage/main.go internal/api/client.go`
→ no matches, and `go test ./cmd/... ./internal/api/ -run 'KeepsUTF8' -v` → both `PASS`.

### Step 5: Full gate

**Verify**: `gofmt -l . && go vet ./... && go test ./... && go test -race ./...`
→ no gofmt output, vet clean, every package `ok` twice.

## Test plan

- `TestStripControls`: table over ASCII, CJK and box-drawing text (kept),
  CSI/OSC escapes, BEL, newline/tab, a C1 control and bidi overrides (removed).
- `TestUntrustedTextIsSanitized`: injected escapes in plan, limit labels,
  breakdown names and model names never reach the output; a newline doesn't
  add a row; width is still respected.
- `TestLabelsNonASCII`: non-ASCII first letters are capitalised correctly in
  `humanize`, `titleWord` and `TierLabel`, and ASCII behaviour is unchanged.
- `TestShortErrKeepsUTF8`, `TestDescribeKeepsUTF8`: truncated messages are
  valid UTF-8 and within the byte budget.
- All existing tests, including every golden, pass unchanged.

## Done criteria

- [ ] `gofmt -l .` prints nothing; `go vet ./...` exits 0
- [ ] `go test ./...` and `go test -race ./...` all `ok`
- [ ] The 5 new tests exist and pass
- [ ] No `.golden` file modified
- [ ] `grep -rn 'ToUpper(s\[:1\])\|ToUpper(w\[:1\])\|msg\[:117\]\|msg\[:57\]' --include=*.go .` → no matches
- [ ] `render.JSON` (`internal/render/render.go`) unchanged
- [ ] `plans/README.md` status row for 003 updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt cannot be found verbatim.
- A golden test fails after Step 1 or Step 3. The fixture is ASCII, so any
  diff means the change altered normal output.
- `TestGoldenANSI` fails, which would mean colour output changed.
- You find text being written to stdout by the text renderer through a path
  other than `line.render`. Report the path rather than adding more filtering
  calls.

## Maintenance notes

- New text output must go through `line`/`seg` and `Lines()` so it gets
  filtered. Never write strings to stdout directly from `render`.
- wtfutil's cmdrunner turns ANSI into tview colour tags, and tview may also
  interpret literal `[colour]` tags in text. Escaping those (tview's
  `[x[]` form) would corrupt plain-terminal output, so it was left out. If it
  ever matters, do it only when `WTF_WIDGET_WIDTH` is set, and test against a
  real wtfutil.
- JSON output intentionally keeps raw strings. A consumer doing
  `jq -r '.limits[].label'` gets them unfiltered and owns that display.
