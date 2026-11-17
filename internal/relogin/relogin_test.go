package relogin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEnviron(t *testing.T) {
	in := []string{
		"HOME=/home/me", "PATH=/bin",
		"ANTHROPIC_API_KEY=sk-ant-api", "ANTHROPIC_AUTH_TOKEN=x", "CLAUDE_CODE_OAUTH_TOKEN=y",
		"CLAUDE_CONFIG_DIR=/elsewhere", "HTTPS_PROXY=http://proxy:3128",
	}
	got := environ(in, "")
	want := []string{"HOME=/home/me", "PATH=/bin", "HTTPS_PROXY=http://proxy:3128"}
	if !slices.Equal(got, want) {
		t.Errorf("default dir: got %q, want %q", got, want)
	}
	got = environ(in, "/work/.claude")
	want = append(want, "CLAUDE_CONFIG_DIR=/work/.claude")
	if !slices.Equal(got, want) {
		t.Errorf("explicit dir: got %q, want %q", got, want)
	}
}

// fakeClaude puts a `claude` script on PATH that records its working
// directory, arguments and environment, then exits with code.
func fakeClaude(t *testing.T, script string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "calls.log")
	body := "#!/bin/sh\n" +
		`{ pwd; echo "cfg=${CLAUDE_CONFIG_DIR-unset} key=${ANTHROPIC_API_KEY-unset}"; for a in "$@"; do printf '[%s]' "$a"; done; echo; } >> "` + log + "\"\n" +
		script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestRun(t *testing.T) {
	log := fakeClaude(t, "exit 0")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api")
	if err := Run(context.Background(), Options{ConfigDir: "/work/.claude", Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("log:\n%s", b)
	}
	realTmp, _ := filepath.EvalSymlinks(tmp)
	if parent, _ := filepath.EvalSymlinks(filepath.Dir(lines[0])); parent != realTmp || !strings.HasPrefix(filepath.Base(lines[0]), "claude-usage-refresh-") {
		t.Errorf("cwd = %q, want a fresh claude-usage-refresh-* dir in %q", lines[0], realTmp)
	}
	if lines[1] != "cfg=/work/.claude key=unset" {
		t.Errorf("env: %s", lines[1])
	}
	if want := "[-p][Reply with just: OK][--model][haiku][--tools][][--no-session-persistence][--strict-mcp-config][--disable-slash-commands][--settings][{\"disableAllHooks\":true}]"; lines[2] != want {
		t.Errorf("args:\n got %s\nwant %s", lines[2], want)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp dir not removed: %v", left)
	}
}

func TestRunErrors(t *testing.T) {
	fakeClaude(t, "exit 3")
	if err := Run(context.Background(), Options{Timeout: 10 * time.Second}); err == nil || err.Error() != "claude failed (exit 3)" {
		t.Errorf("exit 3: %v", err)
	}

	fakeClaude(t, "sleep 5")
	start := time.Now()
	if err := Run(context.Background(), Options{Timeout: 100 * time.Millisecond}); err == nil || err.Error() != "claude timed out" {
		t.Errorf("timeout: %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("timeout took %v", d)
	}

	t.Setenv("PATH", t.TempDir())
	if err := Run(context.Background(), Options{Timeout: time.Second}); err == nil || err.Error() != "claude not found in PATH" {
		t.Errorf("missing: %v", err)
	}
}
