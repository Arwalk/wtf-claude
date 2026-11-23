package main

import (
	"errors"
	"sync"
	"time"

	"github.com/Arwalk/wtf-claude/internal/cache"
	"github.com/Arwalk/wtf-claude/internal/creds"
)

const (
	// reloginTimeout bounds one `claude -p` run.
	reloginTimeout = 60 * time.Second
	// reloginBackoff is how long a failed refresh is not retried, so a login
	// that needs a real `claude` sign-in doesn't start claude on every run.
	reloginBackoff = 30 * time.Minute
)

// loginRefresher implements --refresh-login: after a 401 it has Claude Code
// refresh its own login (see internal/relogin) and returns the new token.
// It makes at most one attempt per run, shared by the usage and profile
// requests, and one at a time across processes.
type loginRefresher struct {
	store cache.Store
	// name is the cache entry that records failed attempts for back-off.
	name string
	// load reads the stored credentials again.
	load func() (creds.Credentials, error)
	// run asks Claude Code to refresh its login.
	run func() error
	now time.Time

	mu    sync.Mutex
	tried bool
	token string
	err   error
}

// refresh returns a token other than rejected, or an error saying why there
// is none.
func (r *loginRefresher) refresh(rejected string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tried {
		r.tried = true
		r.token, r.err = r.attempt(rejected)
	}
	return r.token, r.err
}

// result reports what refresh did: whether it ran, and its error.
func (r *loginRefresher) result() (tried bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tried, r.err
}

func (r *loginRefresher) attempt(rejected string) (string, error) {
	unlock, lerr := r.store.Lock(r.name, reloginTimeout+5*time.Second)
	if lerr == nil {
		defer unlock()
	}
	// Claude Code, or another widget, may have refreshed it meanwhile.
	if tok := r.renewed(rejected); tok != "" {
		return tok, nil
	}
	if errors.Is(lerr, cache.ErrLockTimeout) {
		return "", errors.New("login refresh in progress elsewhere")
	}
	e := r.store.Load(r.name)
	if r.now.Before(e.RetryNotBefore) {
		return "", &cache.ErrBackoff{Last: e.LastError}
	}
	err := r.run()
	if err == nil {
		if tok := r.renewed(rejected); tok != "" {
			_ = r.store.Save(r.name, cache.Entry{})
			return tok, nil
		}
		err = errors.New("claude did not renew the login")
	}
	e.LastErrorAt, e.LastError, e.RetryNotBefore = r.now, err.Error(), r.now.Add(reloginBackoff)
	_ = r.store.Save(r.name, e)
	return "", err
}

// renewed returns the stored token if it is no longer the rejected one.
func (r *loginRefresher) renewed(rejected string) string {
	if cr, err := r.load(); err == nil && cr.AccessToken != rejected {
		return cr.AccessToken
	}
	return ""
}
