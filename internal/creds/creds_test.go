package creds

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const credJSON = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc","refreshToken":"sk-ant-ort01-def","expiresAt":1763300000000,"scopes":["user:inference"]}}`

func opts(t *testing.T, env map[string]string, home string) Options {
	return Options{
		Getenv:   func(k string) string { return env[k] },
		GOOS:     "linux",
		HomeDir:  func() (string, error) { return home, nil },
		Keychain: func(string) ([]byte, error) { t.Fatal("keychain must not be used off darwin"); return nil, nil },
	}
}

func TestLoadPrecedence(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(credJSON), 0o600)
	env := map[string]string{}

	c, err := Load(opts(t, env, home))
	if err != nil || c.AccessToken != "sk-ant-oat01-abc" || !strings.HasSuffix(c.Source, ".credentials.json") {
		t.Fatalf("file: %+v %v", c, err)
	}
	if c.ExpiresAt.UnixMilli() != 1763300000000 || !c.Expired(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expiry: %v", c.ExpiresAt)
	}

	env["CLAUDE_CODE_OAUTH_TOKEN"] = "env-token"
	if c, _ := Load(opts(t, env, home)); c.AccessToken != "env-token" || c.Source != "env" || c.Expired(time.Now()) {
		t.Errorf("env: %+v", c)
	}
	o := opts(t, env, home)
	o.Token = " flag-token "
	if c, _ := Load(o); c.AccessToken != "flag-token" || c.Source != "flag" {
		t.Errorf("flag: %+v", c)
	}
}

func TestConfigDirEnv(t *testing.T) {
	home, alt := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(alt, ".credentials.json"), []byte(credJSON), 0o600)
	env := map[string]string{"CLAUDE_CONFIG_DIR": alt}
	c, err := Load(opts(t, env, home))
	if err != nil || c.Source != filepath.Join(alt, ".credentials.json") {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestNotFoundAndBadFile(t *testing.T) {
	home := t.TempDir()
	if _, err := Load(opts(t, map[string]string{}, home)); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600)
	_, err := Load(opts(t, map[string]string{}, home))
	if err == nil || strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("bad file err = %v", err)
	}
}

func TestKeychainOnDarwin(t *testing.T) {
	home := t.TempDir()
	o := opts(t, map[string]string{}, home)
	o.GOOS = "darwin"
	var asked string
	o.Keychain = func(s string) ([]byte, error) { asked = s; return []byte(credJSON), nil }
	c, err := Load(o)
	if err != nil || c.Source != "keychain" || asked != KeychainService || c.AccessToken != "sk-ant-oat01-abc" {
		t.Fatalf("%+v %v %q", c, err, asked)
	}
	// Keychain miss falls back to the file.
	o.Keychain = func(string) ([]byte, error) { return nil, errors.New("not found") }
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(credJSON), 0o600)
	if c, err := Load(o); err != nil || c.Source == "keychain" {
		t.Errorf("fallback: %+v %v", c, err)
	}
}

func TestLoadNeverWrites(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", ".credentials.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(credJSON), 0o600)
	before, _ := os.Stat(p)
	Load(opts(t, map[string]string{}, home))
	after, _ := os.Stat(p)
	b, _ := os.ReadFile(p)
	if !after.ModTime().Equal(before.ModTime()) || string(b) != credJSON {
		t.Error("credentials file modified")
	}
}

// With a non-default config dir on macOS, the default keychain item belongs
// to another account and must not be used.
func TestKeychainPerConfigDir(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(work, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"WORK"}}`), 0o600)
	o := opts(t, map[string]string{"CLAUDE_CONFIG_DIR": work}, home)
	o.GOOS = "darwin"
	var asked []string
	items := map[string]string{KeychainService: `{"claudeAiOauth":{"accessToken":"PERSONAL"}}`}
	o.Keychain = func(s string) ([]byte, error) {
		asked = append(asked, s)
		if v, ok := items[s]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("not found")
	}
	c, err := Load(o)
	if err != nil || c.AccessToken != "WORK" || c.Source != filepath.Join(work, ".credentials.json") {
		t.Fatalf("got %q from %q (%v); keychain asked %v", c.AccessToken, c.Source, err, asked)
	}
	for _, s := range asked {
		if s == KeychainService {
			t.Errorf("default keychain item queried for a non-default dir: %v", asked)
		}
	}

	// The dir-specific item (as Claude Code names it) wins over the file.
	svc := KeychainServiceFor(work)
	if !strings.HasPrefix(svc, KeychainService+"-") || len(svc) != len(KeychainService)+9 {
		t.Errorf("service name %q", svc)
	}
	items[svc] = `{"claudeAiOauth":{"accessToken":"WORK-KC"}}`
	if c, err := Load(o); err != nil || c.AccessToken != "WORK-KC" || c.Source != "keychain" {
		t.Errorf("dir keychain: %+v %v", c, err)
	}

	// --config-dir behaves like CLAUDE_CONFIG_DIR.
	o.Getenv = func(string) string { return "" }
	o.ConfigDir = work
	if c, _ := Load(o); c.AccessToken != "WORK-KC" {
		t.Errorf("flag dir: %+v", c)
	}

	// Default dir: the plain item is used.
	o.ConfigDir = ""
	if c, _ := Load(o); c.AccessToken != "PERSONAL" {
		t.Errorf("default dir: %+v", c)
	}
}

func TestKeychainServiceForMatchesClaudeCode(t *testing.T) {
	// sha256("/Users/me/.claude-work") starts with 1e91dd84 — computed
	// independently with `printf %s /Users/me/.claude-work | shasum -a 256`.
	if got := KeychainServiceFor("/Users/me/.claude-work"); got != "Claude Code-credentials-"+wantHashPrefix {
		t.Errorf("got %q", got)
	}
}

func TestFileErrorPutsReasonFirst(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{bad`), 0o600)
	_, err := Load(opts(t, map[string]string{}, home))
	var fe *FileError
	if !errors.As(err, &fe) || !strings.HasPrefix(err.Error(), "invalid credentials JSON (") {
		t.Errorf("err = %v", err)
	}
}

const wantHashPrefix = "1e91dd84"
