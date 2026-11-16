// Package creds locates the Claude Code OAuth access token. It only ever
// reads credentials: it never refreshes tokens or writes the credentials
// file, because refreshing rotates the refresh token and would log Claude
// Code out.
package creds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// KeychainService is the macOS keychain item Claude Code stores its
// credentials JSON under for the default config dir (~/.claude).
const KeychainService = "Claude Code-credentials"

// KeychainServiceFor returns the keychain item Claude Code uses for a config
// dir set through CLAUDE_CONFIG_DIR: the default name plus "-" and the first
// 8 hex digits of sha256(dir).
func KeychainServiceFor(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return KeychainService + "-" + hex.EncodeToString(sum[:])[:8]
}

// FileError is a problem with a credentials file. Its message puts the
// reason before the path so it survives truncation in narrow widgets.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return e.Err.Error() + " (" + e.Path + ")" }
func (e *FileError) Unwrap() error { return e.Err }

// Credentials is a resolved access token plus where it came from.
type Credentials struct {
	AccessToken string
	// ExpiresAt is zero when unknown (flag / env tokens).
	ExpiresAt time.Time
	// Source is a short, non-secret description: "flag", "env",
	// "keychain" or the credentials file path.
	Source string
}

// Expired reports whether the token's recorded expiry is in the past.
func (c Credentials) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt)
}

// Options controls credential lookup. Zero values use the environment.
type Options struct {
	// Token, if set, is used as-is (the --token flag).
	Token string
	// ConfigDir overrides $CLAUDE_CONFIG_DIR / ~/.claude.
	ConfigDir string
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// GOOS defaults to runtime.GOOS.
	GOOS string
	// Keychain reads the macOS keychain item; defaults to the `security` CLI.
	Keychain func(service string) ([]byte, error)
	// HomeDir defaults to os.UserHomeDir.
	HomeDir func() (string, error)
}

// ErrNotFound means no credentials were found anywhere.
var ErrNotFound = errors.New("no Claude credentials found; run `claude` and log in (or pass --token / set CLAUDE_CODE_OAUTH_TOKEN)")

// ConfigDir returns the Claude Code configuration directory.
func ConfigDir(o Options) string {
	dir, _ := configDir(o)
	return dir
}

// ConfigDirExplicit reports whether the config dir was chosen explicitly
// (--config-dir or $CLAUDE_CONFIG_DIR) rather than defaulting to ~/.claude.
func ConfigDirExplicit(o Options) bool {
	_, explicit := configDir(o)
	return explicit
}

func configDir(o Options) (string, bool) {
	o = o.withDefaults()
	if o.ConfigDir != "" {
		return o.ConfigDir, true
	}
	if d := strings.TrimSpace(o.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d, true
	}
	home, err := o.HomeDir()
	if err != nil {
		return ".claude", false
	}
	return filepath.Join(home, ".claude"), false
}

// isDefaultDir reports whether dir is ~/.claude.
func isDefaultDir(o Options, dir string) bool {
	home, err := o.HomeDir()
	return err == nil && filepath.Clean(dir) == filepath.Join(home, ".claude")
}

// keychainServices lists the keychain items to try for a config dir, most
// specific first. Claude Code keys the item by a hash of CLAUDE_CONFIG_DIR
// whenever that is set; the plain item belongs to the default dir only, so a
// second account in another dir never picks up the default account's token.
func keychainServices(o Options, dir string, explicit bool) []string {
	var out []string
	if explicit {
		out = append(out, KeychainServiceFor(dir))
		if c := filepath.Clean(dir); c != dir {
			out = append(out, KeychainServiceFor(c))
		}
	}
	if !explicit || isDefaultDir(o, dir) {
		out = append(out, KeychainService)
	}
	return out
}

func (o Options) withDefaults() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Keychain == nil {
		o.Keychain = readKeychain
	}
	if o.HomeDir == nil {
		o.HomeDir = os.UserHomeDir
	}
	return o
}

// Load resolves credentials in this order: --token, $CLAUDE_CODE_OAUTH_TOKEN,
// the macOS keychain item for the config dir (darwin only), then
// <config dir>/.credentials.json.
func Load(o Options) (Credentials, error) {
	o = o.withDefaults()
	if t := strings.TrimSpace(o.Token); t != "" {
		return Credentials{AccessToken: t, Source: "flag"}, nil
	}
	if t := strings.TrimSpace(o.Getenv("CLAUDE_CODE_OAUTH_TOKEN")); t != "" {
		return Credentials{AccessToken: t, Source: "env"}, nil
	}
	dir, explicit := configDir(o)
	var firstErr error
	if o.GOOS == "darwin" {
		for _, svc := range keychainServices(o, dir, explicit) {
			b, err := o.Keychain(svc)
			if err != nil {
				continue
			}
			c, perr := Parse(b)
			if perr == nil {
				c.Source = "keychain"
				return c, nil
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("keychain: %w", perr)
			}
		}
	}
	path := filepath.Join(dir, ".credentials.json")
	b, err := os.ReadFile(path)
	if err == nil {
		c, perr := Parse(b)
		if perr != nil {
			return Credentials{}, &FileError{Path: path, Err: perr}
		}
		c.Source = path
		return c, nil
	}
	if firstErr != nil {
		return Credentials{}, firstErr
	}
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrNotFound
	}
	return Credentials{}, &FileError{Path: path, Err: errors.New("cannot read credentials")}
}

// Parse decodes the credentials JSON written by Claude Code:
// {"claudeAiOauth":{"accessToken":..., "expiresAt": <ms epoch>, ...}}.
func Parse(b []byte) (Credentials, error) {
	var f struct {
		OAuth *struct {
			AccessToken string          `json:"accessToken"`
			ExpiresAt   json.RawMessage `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return Credentials{}, errors.New("invalid credentials JSON")
	}
	if f.OAuth == nil || strings.TrimSpace(f.OAuth.AccessToken) == "" {
		return Credentials{}, errors.New("no OAuth token in credentials (API-key login?)")
	}
	c := Credentials{AccessToken: strings.TrimSpace(f.OAuth.AccessToken)}
	var ms float64
	if err := json.Unmarshal(f.OAuth.ExpiresAt, &ms); err == nil && ms > 0 {
		if ms < 1e12 { // seconds, defensively
			ms *= 1000
		}
		c.ExpiresAt = time.UnixMilli(int64(ms))
	}
	return c, nil
}

func readKeychain(service string) ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(out))), nil
}
