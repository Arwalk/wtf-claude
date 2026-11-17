// Package cache stores API responses on disk so that frequent refreshes (as
// in a wtfutil widget) do not hammer the API, and so that a stale copy can be
// shown when the network or the API is unavailable.
package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one cached response plus fetch bookkeeping.
type Entry struct {
	FetchedAt   time.Time       `json:"fetched_at,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	LastErrorAt time.Time       `json:"last_error_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	// LastErrorAuth records that Policy.IsAuth matched the last error, so
	// runs in back-off can still say it was an auth error.
	LastErrorAuth bool `json:"last_error_auth,omitempty"`
	// RetryNotBefore suppresses network attempts until this time after a
	// failure (honouring Retry-After), so many widgets do not hammer a
	// rate-limited API.
	RetryNotBefore time.Time `json:"retry_not_before,omitempty"`
}

// HasBody reports whether the entry holds a usable response.
func (e Entry) HasBody() bool { return len(e.Body) > 0 && !e.FetchedAt.IsZero() }

// Store is a directory of cache entries.
type Store struct{ Dir string }

// DefaultDir returns $XDG_CACHE_HOME/claude-usage or ~/.cache/claude-usage.
func DefaultDir() string {
	if d := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "claude-usage")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "claude-usage")
	}
	return filepath.Join(os.TempDir(), "claude-usage")
}

func (s Store) path(name string) string { return filepath.Join(s.Dir, name+".json") }

// Load reads an entry. A missing or corrupt file yields a zero Entry.
func (s Store) Load(name string) Entry {
	if s.Dir == "" {
		return Entry{}
	}
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return Entry{}
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		return Entry{}
	}
	return e
}

// Save writes an entry atomically with mode 0600 (directory 0700).
func (s Store) Save(name string, e Entry) error {
	if s.Dir == "" {
		return nil
	}
	if len(e.Body) > 0 && !json.Valid(e.Body) {
		return errors.New("cache: body is not valid JSON")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(name))
}

// Result is the outcome of Get.
type Result struct {
	Body      []byte
	FetchedAt time.Time
	// FromCache is true when Body came from disk (fresh or stale).
	FromCache bool
	// Stale is true when a refresh was needed but failed (or was suppressed
	// by back-off) and an older body is being returned.
	Stale bool
	// Err is the fetch error, if any (set even when a stale body is
	// returned).
	Err error
}

// Policy configures Get.
type Policy struct {
	TTL time.Duration
	// Backoff is how long to wait after a failure before trying again when
	// the server did not say (Retry-After). Zero means 30s.
	Backoff time.Duration
	// RetryAfter extracts a server-requested delay from an error.
	RetryAfter func(error) time.Duration
	// Validate, if set, rejects bodies that should not be cached.
	Validate func([]byte) error
	// Describe turns an error into the short message remembered for
	// back-off reporting. Defaults to err.Error().
	Describe func(error) string
	// IsAuth, if set, reports whether an error is an auth error; the answer
	// is remembered for back-off reporting (ErrBackoff.Auth).
	IsAuth func(error) bool
	// LockWait is how long to wait for another process that is refreshing
	// the same entry. Zero means 5s.
	LockWait time.Duration
}

// ErrBackoff is reported when a fetch was skipped because a recent attempt
// failed. Auth is true when that failure was an auth error (Policy.IsAuth).
type ErrBackoff struct {
	Last string
	Auth bool
}

func (e *ErrBackoff) Error() string {
	if e.Last != "" {
		return e.Last
	}
	return "recent request failed"
}

// ErrBusy is reported when another process held the refresh lock for longer
// than Policy.LockWait and an older cached body is returned instead.
var ErrBusy = errors.New("refresh in progress elsewhere")

// fresh reports whether e can be served without a refresh.
func fresh(e Entry, now time.Time, p Policy) bool {
	return e.HasBody() && p.TTL > 0 && now.Sub(e.FetchedAt) < p.TTL && !e.FetchedAt.After(now.Add(time.Minute))
}

// fromDisk returns the result for e if no fetch is needed (fresh, or in
// back-off after a recent failure).
func fromDisk(e Entry, now time.Time, p Policy) (Result, bool) {
	if fresh(e, now, p) {
		return Result{Body: e.Body, FetchedAt: e.FetchedAt, FromCache: true}, true
	}
	if !e.RetryNotBefore.IsZero() && now.Before(e.RetryNotBefore) {
		err := &ErrBackoff{Last: e.LastError, Auth: e.LastErrorAuth}
		if e.HasBody() {
			return Result{Body: e.Body, FetchedAt: e.FetchedAt, FromCache: true, Stale: true, Err: err}, true
		}
		return Result{Err: err}, true
	}
	return Result{}, false
}

// Get returns a cached body if younger than TTL, otherwise calls fetch and
// caches the result. On failure it falls back to any older cached body.
//
// Refreshes are serialised across processes with an advisory lock (on
// platforms that support it): several widgets refreshing at the same moment
// make one request, and the others pick up its result from disk.
func (s Store) Get(name string, now time.Time, p Policy, fetch func() ([]byte, error)) Result {
	e := s.Load(name)
	if r, ok := fromDisk(e, now, p); ok {
		return r
	}
	wait := p.LockWait
	if wait <= 0 {
		wait = 5 * time.Second
	}
	if unlock, err := s.lock(name, wait); err == nil {
		defer unlock()
		// Another process may have refreshed (or failed) while we waited.
		e = s.Load(name)
		if r, ok := fromDisk(e, now, p); ok {
			return r
		}
	} else if errors.Is(err, ErrLockTimeout) && e.HasBody() {
		return Result{Body: e.Body, FetchedAt: e.FetchedAt, FromCache: true, Stale: true, Err: ErrBusy}
	}

	body, err := fetch()
	if err == nil && p.Validate != nil {
		err = p.Validate(body)
	}
	if err == nil {
		ne := Entry{FetchedAt: now, Body: body}
		_ = s.Save(name, ne)
		return Result{Body: body, FetchedAt: now}
	}

	// Re-read before recording the failure so a newer body saved meanwhile
	// (by a process that could not take the lock) is never overwritten.
	if cur := s.Load(name); cur.HasBody() && (!e.HasBody() || cur.FetchedAt.After(e.FetchedAt)) {
		if fresh(cur, now, p) {
			return Result{Body: cur.Body, FetchedAt: cur.FetchedAt, FromCache: true}
		}
		e = cur
	}
	backoff := p.Backoff
	if backoff <= 0 {
		backoff = 30 * time.Second
	}
	if p.RetryAfter != nil {
		if d := p.RetryAfter(err); d > 0 {
			backoff = min(d, 15*time.Minute)
		}
	}
	e.LastErrorAt = now
	if p.Describe != nil {
		e.LastError = p.Describe(err)
	} else {
		e.LastError = err.Error()
	}
	e.LastErrorAuth = p.IsAuth != nil && p.IsAuth(err)
	e.RetryNotBefore = now.Add(backoff)
	_ = s.Save(name, e)
	if e.HasBody() {
		return Result{Body: e.Body, FetchedAt: e.FetchedAt, FromCache: true, Stale: true, Err: err}
	}
	return Result{Err: err}
}

// ErrLockTimeout is returned by Lock when another process kept the lock.
var ErrLockTimeout = errors.New("cache: timed out waiting for lock")

// Lock takes the same cross-process lock Get takes while refreshing name,
// waiting up to wait. It returns ErrLockTimeout if another process kept it,
// or another error if locking is not possible here (callers then proceed
// unlocked).
func (s Store) Lock(name string, wait time.Duration) (unlock func(), err error) {
	return s.lock(name, wait)
}
