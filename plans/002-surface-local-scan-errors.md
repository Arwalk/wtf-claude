# Plan 002: Report unreadable transcript files/dirs instead of "no activity"

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: this directory was not a git repository when
> the plan was written, so drift is checked by content hash. Run:
> `sha256sum internal/localstats/localstats.go internal/localstats/localstats_test.go internal/render/render.go internal/render/render_test.go cmd/claude-usage/main_test.go | cut -c1-16`
> Expected (as planned):
> ```
> d0e78a6e14a2ca35  (internal/localstats/localstats.go)
> 244bbf6e66220182  (internal/localstats/localstats_test.go)
> 910113491f87dc75  (internal/render/render.go)
> 6e873507aa358d2a  (internal/render/render_test.go)
> 78937c84ffe474fa  (cmd/claude-usage/main_test.go)
> ```
> `cmd/claude-usage/main_test.go` is expected to differ if plan 001 has
> already landed, because it appends tests to that file. For any differing
> hash, compare the "Current state" excerpts below against the live file. If
> they still match verbatim, proceed. If not, STOP.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none. If 001 is also being executed, run 001 first; both
  append to `cmd/claude-usage/main_test.go`.
- **Category**: bug
- **Planned at**: no git repo (content hashes above), 2026-11-16

## Why this matters

`claude-usage` aggregates token stats from Claude Code transcript files
(`~/.claude/projects/**/*.jsonl`). When a transcript directory or file cannot
be read (permission denied, I/O error), the scanner silently skips it. The
directory-walk callback never returns an error, so `Scan`'s error result is
always nil. As a result, the `status.local_error` field documented in the
README's JSON section can never be set. The text view then says "no activity in
the last 7 days", which is wrong: there may be plenty of activity the tool
couldn't read. After this plan, the first real read error is returned and
appears in JSON as `local_error`. If nothing could be counted, the text view
says `local stats unavailable: <reason> (<path>)` instead of claiming no
activity.

## Current state

Files involved:

- `internal/localstats/localstats.go`: transcript discovery (`findFiles`),
  parsing (`parseFile`, `scanReader`) and merging (`Scan`).
  - Lines 121-186, `findFiles`. The error branch of the walk callback (lines
    133-139) swallows every error:
    ```go
    		err = filepath.WalkDir(real, func(path string, d fs.DirEntry, err error) error {
    			if err != nil {
    				if d != nil && d.IsDir() && path != real {
    					return fs.SkipDir
    				}
    				return nil
    			}
    ```
    The callback never returns a non-nil, non-SkipDir error, so this at lines
    178-180 is dead code:
    ```go
    		if err != nil && walkErr == nil {
    			walkErr = err
    		}
    ```
    Also inside `walk`, lines 128-131:
    ```go
    		real, err := filepath.EvalSymlinks(root)
    		if err != nil || visitedDirs[real] {
    			return
    		}
    ```
    and the symlink branch at lines 151-157:
    ```go
    			if d.Type()&fs.ModeSymlink != 0 {
    				// os.Stat follows the link: a directory is walked on its
    				// own, a file is judged by its target's mtime.
    				info, err = os.Stat(path)
    				if err != nil {
    					return nil
    				}
    ```
  - Lines 188-193, `fileResult`:
    ```go
    // fileResult is one parsed transcript.
    type fileResult struct {
    	entries []keyedEntry
    	skipped int
    	ok      bool
    }
    ```
  - Lines 257-268, `parseFile`, which drops the open error:
    ```go
    func parseFile(path string, since time.Time) fileResult {
    	f, err := os.Open(path)
    	if err != nil {
    		return fileResult{}
    	}
    ```
  - Lines 232-237 in `Scan`, the merge loop, which ignores failed files:
    ```go
    	byKey := map[string]int{}
    	for _, fr := range results {
    		if !fr.ok {
    			continue
    		}
    ```
    and line 254: `return res, walkErr`
- `cmd/claude-usage/main.go` lines 392-394 already propagate the error. This
  file needs NO change:
  ```go
  		if scanErr != nil {
  			status.LocalError = shortErr(scanErr)
  		}
  ```
  `shortErr` truncates the message to 120 bytes. That is why the error message
  must put the reason before the path (see conventions).
- `internal/render/render.go`:
  - Line 144-146 in `full`:
    ```go
    	if r.Local != nil && rd.trim < trimLocal {
    		rd.local(r.Local)
    	}
    ```
  - Lines 179-181 in `compact`:
    ```go
    	if len(rd.lines) == 0 && r.Local != nil {
    		rd.local(r.Local)
    	}
    ```
  - Lines 703-711, start of `local`:
    ```go
    func (rd *renderer) local(st *localstats.Stats) {
    	if rd.trim < trimLocalHeader {
    		header := part{[]string{"Local Claude Code", "Local stats", "Local"}, dim | bold}
    		rd.add(line{{header.bestFit(rd.w), header.st}})
    	}
    	if st.Today.Messages == 0 && st.Week.Messages == 0 {
    		rd.add(line{{fitText("no activity in the last 7 days", rd.w), dim}})
    		return
    	}
    ```
  - Lines 644-645, the existing pattern for an error line in `status`:
    ```go
    	case s.UsageError != "" && len(r.Limits) == 0:
    		rd.add(line{{fitText("usage unavailable: "+s.UsageError, rd.w), red}})
    ```

Conventions to match:

- **Error messages put the reason first, then the path**, so they survive
  truncation in narrow widgets. Exemplar: `internal/creds/creds.go:33-41`:
  ```go
  // FileError is a problem with a credentials file. Its message puts the
  // reason before the path so it survives truncation in narrow widgets.
  type FileError struct {
  	Path string
  	Err  error
  }

  func (e *FileError) Error() string { return e.Err.Error() + " (" + e.Path + ")" }
  func (e *FileError) Unwrap() error { return e.Err }
  ```
- A missing file or dir (`fs.ErrNotExist`) is NOT an error here. Claude Code
  creates and deletes transcripts while the scan runs, and dangling symlinks
  are tolerated by design. Only report other errors (permission denied, I/O).
- Partial results are still returned. An error never discards the entries that
  were read.
- Standard library only; `gofmt`; doc comment on every exported identifier.
- Tests use `t.TempDir()` and the `writeFile`/`asst` helpers at the top of
  `internal/localstats/localstats_test.go`.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Format check | `gofmt -l .` | no output |
| Vet | `go vet ./...` | exit 0 |
| Tests | `go test ./...` | all `ok` |
| Race | `go test -race ./internal/localstats/` | `ok` |
| Package tests | `go test ./internal/localstats/ ./internal/render/ -v -run '<name>'` | named tests PASS |

## Scope

**In scope** (the only files you should modify):
- `internal/localstats/localstats.go`
- `internal/localstats/localstats_test.go`
- `internal/render/render.go`
- `internal/render/render_test.go`
- `cmd/claude-usage/main_test.go` (append one test)
- `README.md`: one sentence, Step 5 only

**Out of scope** (do NOT touch):
- `internal/render/testdata/*.golden`. The fixture has no local error, so the
  golden output must not change. Never run `go test ./internal/render -update`
  for this plan.
- `cmd/claude-usage/main.go`. It already copies the error into
  `status.LocalError`.
- `internal/report/report.go`. `Status.LocalError` already exists with JSON
  tag `local_error`.
- Showing a note when *some* files failed but other activity was counted.
  Deferred (see Maintenance notes). Text output changes only when nothing at
  all was counted.
- The skipped-lines counter (`LinesSkipped`) and `scanReader`'s read-error
  handling.

## Git workflow

- If the directory is a git repository when you execute, create branch
  `advisor/002-local-scan-errors` and commit once at the end with message
  `Report unreadable transcripts instead of "no activity"`. Otherwise edit in
  place.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add `ScanError` and record walk errors

In `internal/localstats/localstats.go`:

1. After the `ScanResult` type (ends at line 115), add:
   ```go
   // ScanError is a transcript file or directory that could not be read. Its
   // message puts the reason before the path so it survives truncation in
   // narrow widgets.
   type ScanError struct {
   	Path string
   	Err  error
   }

   func (e *ScanError) Error() string {
   	reason := e.Err
   	var pe *fs.PathError
   	if errors.As(e.Err, &pe) {
   		reason = pe.Err // avoid repeating the path
   	}
   	return reason.Error() + " (" + e.Path + ")"
   }

   func (e *ScanError) Unwrap() error { return e.Err }

   // readErr wraps err as a *ScanError, or returns nil for errors that are
   // expected while Claude Code is writing (files or links that vanished).
   func readErr(path string, err error) error {
   	if err == nil || errors.Is(err, fs.ErrNotExist) {
   		return nil
   	}
   	return &ScanError{Path: path, Err: err}
   }
   ```
   `errors` and `io/fs` are already imported.
2. In `findFiles`, add a helper right after `var walkErr error`:
   ```go
   	note := func(path string, err error) {
   		if e := readErr(path, err); e != nil && walkErr == nil {
   			walkErr = e
   		}
   	}
   ```
3. In `walk`, change lines 128-131 to record the EvalSymlinks error but keep
   the early return:
   ```go
   		real, err := filepath.EvalSymlinks(root)
   		if err != nil {
   			note(root, err)
   			return
   		}
   		if visitedDirs[real] {
   			return
   		}
   ```
4. In the WalkDir callback's error branch (lines 134-139), call `note(path, err)`
   as the first statement inside `if err != nil {`. Keep the existing
   `SkipDir`/`nil` returns unchanged.
5. In the symlink branch (lines 154-157), change `if err != nil { return nil }`
   to `if err != nil { note(path, err); return nil }`.
6. Delete the now-redundant block that sets `walkErr` from WalkDir's return
   (lines 178-180), but keep the effect: replace
   ```go
   		if err != nil && walkErr == nil {
   			walkErr = err
   		}
   ```
   with
   ```go
   		note(real, err)
   ```
   (`readErr` ignores nil.)

Leave the subdirectory `EvalSymlinks` failure inside the callback
(`if err != nil || visitedDirs[rp] { return fs.SkipDir }`) silent. Do not
change it.

**Verify**: `go vet ./internal/localstats/ && go test ./internal/localstats/`
→ `ok`. `TestScanFollowsSymlinks` must still pass: symlink loops and
duplicate links are not errors.

### Step 2: Record file-open errors

1. Add a field to `fileResult`:
   ```go
   	err     error // open failure (already filtered through readErr)
   ```
2. In `parseFile`, change the open failure to:
   ```go
   	if err != nil {
   		return fileResult{err: readErr(path, err)}
   	}
   ```
3. In `Scan`, keep one error: the first directory/walk error if there is one,
   otherwise the first file-open error in walk order. Change the start of the
   merge loop to:
   ```go
   	firstErr := walkErr
   	byKey := map[string]int{}
   	for _, fr := range results {
   		if !fr.ok {
   			if firstErr == nil && fr.err != nil {
   				firstErr = fr.err
   			}
   			continue
   		}
   ```
   and the final line to `return res, firstErr`.
4. Update `Scan`'s doc comment (lines 200-209). Append this sentence: "The
   error, if any, is the first file or directory that could not be read (a
   *ScanError); entries from everything that could be read are still
   returned."

**Verify**: `go test -race ./internal/localstats/` → `ok`.

### Step 3: Tests for the scanner

Append to `internal/localstats/localstats_test.go`. Add `errors`, `io/fs` and
`runtime` to the imports if missing.

```go
func skipIfPermsIgnored(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
}

// Unreadable files and directories are reported (reason first) while
// everything readable is still counted.
func TestScanReportsUnreadable(t *testing.T) {
	skipIfPermsIgnored(t)
	dir := t.TempDir()
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(dir, "ok", "a.jsonl"), asst("2026-11-16T10:00:00Z", "s1", "r1", "m1", "claude-x", 1, 1, 1, 1))

	locked := filepath.Join(dir, "locked")
	writeFile(t, filepath.Join(locked, "b.jsonl"), asst("2026-11-16T10:00:00Z", "s2", "r2", "m2", "claude-x", 1, 1, 1, 1))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	res, err := Scan([]string{dir}, since)
	if len(res.Entries) != 1 {
		t.Errorf("readable entries = %d, want 1", len(res.Entries))
	}
	var se *ScanError
	if !errors.As(err, &se) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want *ScanError wrapping permission denied", err)
	}
	if !strings.HasPrefix(err.Error(), "permission denied (") || !strings.Contains(err.Error(), "locked") {
		t.Errorf("message = %q, want reason first then path", err.Error())
	}
}

func TestScanReportsUnreadableFile(t *testing.T) {
	skipIfPermsIgnored(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "p", "a.jsonl")
	writeFile(t, p, asst("2026-11-16T10:00:00Z", "s1", "r1", "m1", "claude-x", 1, 1, 1, 1))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	_, err := Scan([]string{dir}, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want permission denied", err)
	}
}

// Vanished files and dangling links are normal while Claude Code runs.
func TestScanIgnoresMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone.jsonl"), filepath.Join(dir, "dangling.jsonl")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := Scan([]string{dir, filepath.Join(dir, "does-not-exist")}, time.Time{}); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}
```

**Verify**: `go test ./internal/localstats/ -v -run 'Unreadable|IgnoresMissing|FollowsSymlinks'`
→ all four tests `PASS`. As root they show `SKIP` instead, which is
acceptable: report it.

### Step 4: Show the error when nothing could be counted

In `internal/render/render.go`:

1. Change the signature of `local` to
   `func (rd *renderer) local(st *localstats.Stats, localErr string)`.
2. Replace the "no activity" block (lines 708-711) with:
   ```go
   	if st.Today.Messages == 0 && st.Week.Messages == 0 {
   		if localErr != "" {
   			rd.add(line{{fitText("local stats unavailable: "+localErr, rd.w), red}})
   			return
   		}
   		rd.add(line{{fitText("no activity in the last 7 days", rd.w), dim}})
   		return
   	}
   ```
3. Update both call sites: `rd.local(r.Local, r.Status.LocalError)` in `full`
   (line 145) and in `compact` (line 180).

Append to `internal/render/render_test.go`, modelled on `TestStaleAndErrorNotes`:

```go
func TestLocalErrorShown(t *testing.T) {
	r := report.Build(report.Input{Now: fixedNow, Local: &localstats.Stats{},
		Status: report.Status{LocalError: "permission denied (/home/u/.claude/projects/p)"}})
	got := renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "local stats unavailable: permission denied") || strings.Contains(got, "no activity") {
		t.Errorf("want the error instead of 'no activity':\n%s", got)
	}
	// With data the stats are shown as usual (the error stays JSON-only).
	r = report.Build(report.Input{Now: fixedNow, Local: sampleLocal(),
		Status: report.Status{LocalError: "permission denied (/x)"}})
	got = renderString(t, r, Options{Width: 60, Loc: cet})
	if !strings.Contains(got, "Today") || strings.Contains(got, "unavailable") {
		t.Errorf("partial error must not hide stats:\n%s", got)
	}
}
```

Before editing in this step, record the golden hashes:
`sha256sum internal/render/testdata/* > /tmp/goldens-before.txt`.

**Verify**: `go test ./internal/render/` → `ok`. That includes every
`TestGolden*`. Also `sha256sum -c /tmp/goldens-before.txt` → every line
`OK`, which proves no golden file was modified.

### Step 5: End-to-end JSON test and README note

1. Append to `cmd/claude-usage/main_test.go`, modelled on
   `TestExplicitConfigDirLocalStats` (line 388):
   ```go
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
   ```
   Add `"runtime"` to that file's imports if it is not there.
2. In `README.md`, the `local` row of the "What it shows" table (line 40) ends
   with "...and the top models by tokens. |". Insert this before the closing
   ` |`: ` If transcripts can't be read and nothing could be counted, it says
   so instead of reporting no activity.`

**Verify**: `go test ./cmd/... -run LocalErrorInJSON -v` → `PASS` (or `SKIP`
as root), then `gofmt -l . && go vet ./... && go test ./...` → no gofmt
output, all `ok`.

## Test plan

- `TestScanReportsUnreadable`: unreadable subdirectory → `*ScanError`
  wrapping `fs.ErrPermission`, message `permission denied (<path>)`, and the
  readable file is still counted.
- `TestScanReportsUnreadableFile`: unreadable `.jsonl` file → permission error.
- `TestScanIgnoresMissing`: dangling symlink plus a non-existent root → nil
  error, which preserves existing behaviour.
- `TestLocalErrorShown` (render): the error replaces "no activity" when
  nothing was counted; stats render normally when some were.
- `TestLocalErrorInJSON` (cmd): end-to-end, `status.local_error` is populated.
- Existing `TestScanFollowsSymlinks` (asserts `err == nil` with a symlink loop)
  and all golden tests must pass unchanged.

## Done criteria

- [ ] `gofmt -l .` prints nothing; `go vet ./...` exits 0
- [ ] `go test ./...` all `ok`; `go test -race ./internal/localstats/` `ok`
- [ ] The 5 new tests exist and pass (or skip only when running as root / on Windows)
- [ ] `grep -n "rd.local(r.Local, r.Status.LocalError)" internal/render/render.go` → 2 matches
- [ ] No `.golden` file modified
- [ ] `cmd/claude-usage/main.go` not modified by this plan
- [ ] `plans/README.md` status row for 002 updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt does not match the live code.
- Any golden test fails. This plan must not change rendering of the fixture.
- `TestScanFollowsSymlinks` or `TestScanParallelDeterministic` starts failing
  or returns an error. That would mean normal symlink handling is now reported
  as an error, so the filtering in `readErr` is wrong.
- You find that `status.LocalError` is set anywhere other than
  `cmd/claude-usage/main.go:392-394` (the plan assumes that is the only
  producer).

## Maintenance notes

- Partial failures (some files unreadable, some counted) appear only in JSON
  `local_error`. A dim one-line text note was deliberately deferred. It would
  cost a row in height-limited widgets and needs a slot in the trim order
  (`trimLocalHeader` … `trimLocal` in `render.go`).
- Only the *first* error is kept. If users report confusing messages, consider
  a count of unreadable files in `localstats.Stats` (a JSON contract addition:
  update README's JSON section too).
- Reviewers: check that `fs.ErrNotExist` stays filtered (`readErr`). Reporting
  vanished files would flash errors in the widget whenever Claude Code rotates
  transcripts.
