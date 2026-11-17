// Package relogin asks Claude Code to refresh its own login by running the
// cheapest possible `claude -p` request. Claude Code refreshes an expired
// OAuth token before it sends a request and stores the new one where
// creds.Load finds it.
//
// This package never reads, refreshes or writes the token itself: refreshing
// rotates the refresh token, so only Claude Code, which owns the
// credentials, may do it.
package relogin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Prompt is the request sent to Claude Code. The reply is discarded.
const Prompt = "Reply with just: OK"

// args runs Prompt on Haiku with no tools, MCP servers, hooks, skills or
// saved session, so the request is as small and side-effect free as
// possible.
func args() []string {
	return []string{"-p", Prompt, "--model", "haiku", "--tools", "",
		"--no-session-persistence", "--strict-mcp-config", "--disable-slash-commands",
		"--settings", `{"disableAllHooks":true}`}
}

// Options controls Run.
type Options struct {
	// ConfigDir is passed to claude as CLAUDE_CONFIG_DIR. Leave it empty for
	// the default ~/.claude: on macOS, Claude Code picks its keychain item
	// by whether CLAUDE_CONFIG_DIR is set, even when it names ~/.claude.
	ConfigDir string
	// Timeout bounds the whole claude run.
	Timeout time.Duration
}

// Run runs claude once in a new, empty temporary directory, which is
// removed afterwards. Its output is discarded, never shown or logged.
func Run(ctx context.Context, o Options) error {
	path, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	// A directory of our own (0700, random name): Claude Code skips the
	// workspace trust check with -p, so it must not run where someone else
	// could have planted project settings.
	dir, err := os.MkdirTemp("", "claude-usage-refresh-")
	if err != nil {
		return fmt.Errorf("cannot create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args()...)
	cmd.Dir = dir
	cmd.Env = environ(os.Environ(), o.ConfigDir)
	// Stdin, Stdout and Stderr stay nil, i.e. the null device.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("claude timed out")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("claude failed (exit %d)", ee.ExitCode())
	}
	if err != nil {
		return errors.New("cannot run claude")
	}
	return nil
}

// dropped are variables that would make claude authenticate with something
// other than the stored login (and then leave that login expired), plus
// CLAUDE_CONFIG_DIR, which environ sets from configDir.
var dropped = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}

func environ(env []string, configDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, d := range dropped {
			// Case-insensitive, as on Windows.
			drop = drop || strings.EqualFold(name, d)
		}
		if !drop {
			out = append(out, kv)
		}
	}
	if configDir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return out
}
