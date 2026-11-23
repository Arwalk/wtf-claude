package localstats

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func asst(ts, session, req, msgID, model string, in, out, cc, cr int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"requestId":%q,"message":{"id":%q,"model":%q,"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"service_tier":"standard"}}}`,
		ts, session, req, msgID, model, in, out, cc, cr)
}

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanDedupeAndMalformed(t *testing.T) {
	dir := t.TempDir()
	long := `{"type":"user","timestamp":"2026-11-16T10:00:00Z","message":{"content":"` + strings.Repeat("x", 3<<20) + `"}}`
	writeFile(t, filepath.Join(dir, "proj-a", "s1.jsonl"),
		`{"type":"user","timestamp":"2026-11-16T09:59:00Z","message":{"role":"user","content":"hello"}}`,
		// Same message streamed as three content blocks: must count once,
		// keeping the most complete usage.
		asst("2026-11-16T10:00:00Z", "s1", "req_1", "msg_1", "claude-opus-5-5", 10, 1, 100, 1000),
		asst("2026-11-16T10:00:01Z", "s1", "req_1", "msg_1", "claude-opus-5-5", 10, 50, 100, 1000),
		asst("2026-11-16T10:00:02Z", "s1", "req_1", "msg_1", "claude-opus-5-5", 10, 50, 100, 1000),
		`{"type":"assistant","timestamp":"2026-11-16T10:01:00Z","message":{"usage":{"input_tokens":`, // truncated
		`not json at all "assistant" "usage"`,
		long,
		asst("2026-11-16T10:02:00Z", "s1", "req_2", "msg_2", "<synthetic>", 0, 0, 0, 0),
		asst("2026-11-16T10:03:00Z", "s1", "req_3", "msg_3", "claude-sonnet-4-5", 5, 20, 0, 500),
		asst("2026-11-01T10:03:00Z", "s1", "req_old", "msg_old", "claude-sonnet-4-5", 5, 20, 0, 500), // before since
		``,
	)
	// Subagent transcript in a nested dir, repeating msg_1 (must dedupe
	// across files) plus its own message in another session.
	writeFile(t, filepath.Join(dir, "proj-a", "s1", "subagents", "agent-1.jsonl"),
		asst("2026-11-16T10:00:03Z", "s1", "req_1", "msg_1", "claude-opus-5-5", 10, 50, 100, 1000),
		asst("2026-11-16T11:00:00Z", "s2", "req_4", "msg_4", "claude-opus-5-5", 1, 2, 3, 4),
	)
	writeFile(t, filepath.Join(dir, "proj-b", "notes.txt"), asst("2026-11-16T11:00:00Z", "s9", "r", "m", "x", 1, 1, 1, 1))

	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	res, err := Scan([]string{dir, filepath.Join(dir, "missing")}, since)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesScanned != 2 {
		t.Errorf("files = %d", res.FilesScanned)
	}
	if res.LinesSkipped != 2 {
		t.Errorf("skipped = %d, want 2", res.LinesSkipped)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(res.Entries), res.Entries)
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	st := Compute(res.Entries, now, time.UTC, time.Time{})
	w := st.Week
	if w.Messages != 3 || w.Sessions != 2 {
		t.Errorf("week msgs=%d sessions=%d", w.Messages, w.Sessions)
	}
	if w.Tokens.Output != 50+20+2 || w.Tokens.Input != 16 || w.Tokens.CacheCreation != 103 || w.Tokens.CacheRead != 1504 {
		t.Errorf("tokens = %+v", w.Tokens)
	}
	if w.Tokens.Total != w.Tokens.Input+w.Tokens.Output+w.Tokens.CacheCreation+w.Tokens.CacheRead {
		t.Error("total mismatch")
	}
	if len(w.Models) != 2 || w.Models[0].Model != "claude-opus-5-5" || w.Models[0].Messages != 2 {
		t.Errorf("models = %+v", w.Models)
	}
	if s := w.Models[0].Share + w.Models[1].Share; s < 0.999 || s > 1.001 {
		t.Errorf("shares sum to %v", s)
	}
	if st.WeekFromAPI || w.Label != "7d" {
		t.Errorf("rolling week expected: %+v", st)
	}
}

func TestTimezoneDayBoundary(t *testing.T) {
	// UTC+10: local midnight on Nov 17 is 14:00Z on Nov 16.
	loc := time.FixedZone("AEST", 10*3600)
	entries := []Entry{
		{Time: time.Date(2026, 11, 16, 13, 59, 0, 0, time.UTC), SessionID: "a", Model: "m", Tokens: Tokens{Output: 1, Total: 1}},
		{Time: time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC), SessionID: "b", Model: "m", Tokens: Tokens{Output: 10, Total: 10}},
		{Time: time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC), SessionID: "b", Model: "m", Tokens: Tokens{Output: 100, Total: 100}},
	}
	now := time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC) // 07:00 Nov 17 local
	st := Compute(entries, now, loc, time.Time{})
	if st.Today.Messages != 2 || st.Today.Tokens.Output != 110 || st.Today.Sessions != 1 {
		t.Errorf("AEST today = %+v", st.Today)
	}
	// In UTC all three are "today".
	if st := Compute(entries, now, time.UTC, time.Time{}); st.Today.Messages != 3 {
		t.Errorf("UTC today = %+v", st.Today)
	}
	if got := StartOfDay(now, loc); !got.Equal(time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("StartOfDay = %v", got)
	}
}

func TestWeekFromAPI(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Time: now.Add(-3 * 24 * time.Hour), Tokens: Tokens{Output: 5}},
		{Time: now.Add(-time.Hour), Tokens: Tokens{Output: 7}},
	}
	st := Compute(entries, now, time.UTC, now.Add(-2*24*time.Hour))
	if !st.WeekFromAPI || st.Week.Messages != 1 || st.Week.Tokens.Total != 7 {
		t.Errorf("week = %+v", st.Week)
	}
	if st.Week.Models[0].Model != "unknown" {
		t.Errorf("model = %+v", st.Week.Models)
	}
}

func TestMtimeSkip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "old.jsonl")
	writeFile(t, p, asst("2026-11-16T10:00:00Z", "s", "r", "m", "claude-x", 1, 1, 1, 1))
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, _ := Scan([]string{dir}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if res.FilesScanned != 0 || len(res.Entries) != 0 {
		t.Errorf("old file should be skipped: %+v", res)
	}
}

func TestNoTrailingNewline(t *testing.T) {
	var out []keyedEntry
	skipped := scanReader(strings.NewReader(asst("2026-11-16T10:00:00Z", "s", "r", "m", "claude-x", 1, 2, 3, 4)), time.Time{}, map[string]int{}, &out)
	if skipped != 0 || len(out) != 1 || out[0].e.Tokens.Total != 10 {
		t.Errorf("got %+v skipped=%d", out, skipped)
	}
}

// Symlinked transcripts are judged by their target's mtime, and symlinked
// project directories are followed (without looping or double counting).
func TestScanFollowsSymlinks(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	projects := filepath.Join(root, "projects")
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 1. File symlink created long ago pointing at a live transcript.
	target := filepath.Join(other, "live.jsonl")
	writeFile(t, target, asst("2026-11-16T10:00:00Z", "s1", "r1", "m1", "claude-x", 1, 1, 1, 1))
	os.MkdirAll(filepath.Join(projects, "p"), 0o755)
	link := filepath.Join(projects, "p", "s.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := lchtimes(link, old); err != nil {
		t.Logf("cannot set link mtime (%v); test still checks following", err)
	} else if fi, _ := os.Lstat(link); !fi.ModTime().Equal(old) {
		t.Logf("link mtime is %v", fi.ModTime())
	}
	// 2. Symlinked project directory on another disk.
	moved := filepath.Join(other, "moved-project")
	writeFile(t, filepath.Join(moved, "s2.jsonl"), asst("2026-11-16T11:00:00Z", "s2", "r2", "m2", "claude-x", 1, 2, 3, 4))
	os.Symlink(moved, filepath.Join(projects, "proj-linked"))
	// 3. A loop and a second link to the same file must not double count.
	os.Symlink(projects, filepath.Join(projects, "p", "loop"))
	os.Symlink(target, filepath.Join(projects, "p", "again.jsonl"))

	res, err := Scan([]string{projects}, since)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesScanned != 2 || len(res.Entries) != 2 {
		t.Errorf("files=%d entries=%+v", res.FilesScanned, res.Entries)
	}
}

// Parallel parsing must give the same result as a sequential scan,
// including which duplicate's usage wins across files.
func TestScanParallelDeterministic(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 40; i++ {
		var lines []string
		for j := 0; j < 30; j++ {
			// Message j repeats in every file with growing output; the
			// first file that sees the highest output must win.
			lines = append(lines, asst("2026-11-16T10:00:00Z", fmt.Sprintf("s%d", i), fmt.Sprintf("r%d", j), fmt.Sprintf("m%d", j), "claude-x", 1, i%7, 0, 0))
		}
		lines = append(lines, asst("2026-11-16T10:00:00Z", "own", fmt.Sprintf("own%d", i), fmt.Sprintf("own%d", i), "claude-y", 1, 1, 0, 0))
		writeFile(t, filepath.Join(dir, fmt.Sprintf("p%02d", i), "s.jsonl"), lines...)
	}
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	first, err := Scan([]string{dir}, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 70 || first.FilesScanned != 40 {
		t.Fatalf("entries=%d files=%d", len(first.Entries), first.FilesScanned)
	}
	for _, e := range first.Entries[:30] {
		// Message keys first appear in p00 (session s0) with the max
		// output (6) found later in p06.
		if e.SessionID != "s0" || e.Tokens.Output != 6 {
			t.Fatalf("entry %+v", e)
		}
	}
	for n := 0; n < 5; n++ {
		again, _ := Scan([]string{dir}, since)
		if fmt.Sprint(again.Entries) != fmt.Sprint(first.Entries) {
			t.Fatal("scan result is not deterministic")
		}
	}
}

// An explicit config dir (a separate account) must not pull in the default
// ~/.claude transcripts.
func TestDefaultDirsExplicit(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755)
	os.MkdirAll(filepath.Join(home, ".config", "claude", "projects"), 0o755)
	os.MkdirAll(filepath.Join(work, "projects"), 0o755)
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	got := DefaultDirs(filepath.Join(work), true)
	if len(got) != 1 || got[0] != real(filepath.Join(work, "projects")) {
		t.Errorf("explicit: %v", got)
	}
	got = DefaultDirs(filepath.Join(home, ".claude"), false)
	if len(got) != 2 {
		t.Errorf("default: %v", got)
	}
}

// lchtimes sets a symlink's own mtime, like `touch -h`.
func lchtimes(path string, t time.Time) error {
	return exec.Command("touch", "-h", "-d", t.UTC().Format("2006-01-02T15:04:05Z"), path).Run()
}

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
