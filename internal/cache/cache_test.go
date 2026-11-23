package cache

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestGetFreshStaleAndBackoff(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sub")}
	calls := 0
	ok := func() ([]byte, error) { calls++; return []byte(`{"n":1}`), nil }
	fail := func() ([]byte, error) { calls++; return nil, errors.New("boom") }
	p := Policy{TTL: 2 * time.Minute}

	r := s.Get("usage", t0, p, ok)
	if r.Err != nil || r.FromCache || string(r.Body) != `{"n":1}` || calls != 1 {
		t.Fatalf("first: %+v calls=%d", r, calls)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no permission bits to check.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode: %v", fi.Mode())
	}
	if di, _ := os.Stat(s.Dir); runtime.GOOS != "windows" && di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode().Perm())
	}

	// Within TTL: served from cache, no call.
	r = s.Get("usage", t0.Add(time.Minute), p, fail)
	if !r.FromCache || r.Stale || calls != 1 {
		t.Fatalf("fresh: %+v calls=%d", r, calls)
	}

	// Expired + failure: stale body with error.
	r = s.Get("usage", t0.Add(5*time.Minute), p, fail)
	if !r.Stale || r.Err == nil || string(r.Body) != `{"n":1}` || !r.FetchedAt.Equal(t0) || calls != 2 {
		t.Fatalf("stale: %+v calls=%d", r, calls)
	}

	// Inside back-off window: no network call, still stale.
	r = s.Get("usage", t0.Add(5*time.Minute+10*time.Second), p, ok)
	var be *ErrBackoff
	if !r.Stale || !errors.As(r.Err, &be) || calls != 2 {
		t.Fatalf("backoff: %+v calls=%d", r, calls)
	}

	// After back-off: refetch succeeds and clears error state.
	r = s.Get("usage", t0.Add(6*time.Minute), p, ok)
	if r.Err != nil || r.Stale || calls != 3 {
		t.Fatalf("recovered: %+v calls=%d", r, calls)
	}
	if e := s.Load("usage"); e.LastError != "" || !e.RetryNotBefore.IsZero() {
		t.Errorf("error state not cleared: %+v", e)
	}
}

func TestGetNoCacheError(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	r := s.Get("x", t0, Policy{TTL: time.Minute, RetryAfter: func(error) time.Duration { return 5 * time.Minute },
		Describe: func(error) string { return "rate limited (429)" }}, func() ([]byte, error) { return nil, errors.New("429") })
	if r.Body != nil || r.Err == nil {
		t.Fatalf("%+v", r)
	}
	e := s.Load("x")
	if !e.RetryNotBefore.Equal(t0.Add(5*time.Minute)) || e.LastError != "rate limited (429)" {
		t.Errorf("entry = %+v", e)
	}
	r = s.Get("x", t0.Add(time.Minute), Policy{TTL: time.Minute}, func() ([]byte, error) { t.Fatal("should not fetch"); return nil, nil })
	if r.Err == nil || r.Err.Error() != "rate limited (429)" {
		t.Errorf("backoff err = %v", r.Err)
	}
}

func TestValidateAndTTLZero(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	calls := 0
	f := func() ([]byte, error) { calls++; return []byte(`{}`), nil }
	s.Get("y", t0, Policy{TTL: 0}, f)
	s.Get("y", t0, Policy{TTL: 0}, f)
	if calls != 2 {
		t.Errorf("TTL 0 must always fetch, calls=%d", calls)
	}
	r := s.Get("z", t0, Policy{TTL: time.Minute, Validate: func([]byte) error { return errors.New("bad") }}, func() ([]byte, error) { return []byte(`[]`), nil })
	if r.Err == nil || r.Body != nil {
		t.Errorf("invalid body accepted: %+v", r)
	}
}

func TestCorruptCacheIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.json"), []byte("{nope"), 0o600)
	if e := (Store{Dir: dir}).Load("c"); e.HasBody() {
		t.Errorf("corrupt cache returned %+v", e)
	}
}

// Two refreshers racing on an expired entry (two wtfutil widgets on the same
// tick) must make a single request; the second one reads the first one's
// result from disk.
func TestConcurrentGetFetchesOnce(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if unlock, err := s.Lock("probe", 0); err != nil {
		t.Skipf("no cross-process lock here: %v", err)
	} else {
		unlock()
	}
	var calls atomic.Int32
	fetch := func() ([]byte, error) {
		calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		return []byte(`{"n":1}`), nil
	}
	var wg sync.WaitGroup
	results := make([]Result, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.Get("usage", t0, Policy{TTL: time.Minute}, fetch)
		}(i)
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
	for i, r := range results {
		if string(r.Body) != `{"n":1}` || r.Err != nil || r.Stale {
			t.Errorf("result %d = %+v", i, r)
		}
	}
}

// A run whose fetch fails must not overwrite a newer body that another
// process saved while the fetch was in flight.
func TestFailedRunKeepsNewerBody(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p := Policy{TTL: time.Minute}
	s.Save("usage", Entry{FetchedAt: t0, Body: []byte(`{"v":"old"}`)})
	now := t0.Add(5 * time.Minute)
	r := s.Get("usage", now, p, func() ([]byte, error) {
		// Simulates a process that saved without the lock (e.g. after a
		// lock timeout) while this fetch was running.
		s.Save("usage", Entry{FetchedAt: now.Add(time.Second), Body: []byte(`{"v":"new"}`)})
		return nil, errors.New("429")
	})
	if string(r.Body) != `{"v":"new"}` || r.Stale || r.Err != nil {
		t.Errorf("result = %+v", r)
	}
	if e := s.Load("usage"); string(e.Body) != `{"v":"new"}` {
		t.Errorf("cache holds %s", e.Body)
	}

	// Newer but already expired body: the failure is recorded on top of it.
	s.Save("usage", Entry{FetchedAt: t0, Body: []byte(`{"v":"old"}`)})
	later := t0.Add(10 * time.Minute)
	r = s.Get("usage", later, p, func() ([]byte, error) {
		s.Save("usage", Entry{FetchedAt: t0.Add(time.Minute), Body: []byte(`{"v":"mid"}`)})
		return nil, errors.New("boom")
	})
	e := s.Load("usage")
	if string(r.Body) != `{"v":"mid"}` || !r.Stale || string(e.Body) != `{"v":"mid"}` || e.LastError != "boom" {
		t.Errorf("result = %+v entry = %+v", r, e)
	}
}

// While another process holds the lock past LockWait, an older body is
// returned as stale instead of making a second request.
func TestLockTimeoutServesStale(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	s.Save("usage", Entry{FetchedAt: t0, Body: []byte(`{"v":1}`)})
	unlock, err := s.lock("usage", time.Second)
	if err != nil {
		t.Skipf("locking unsupported: %v", err)
	}
	defer unlock()
	r := s.Get("usage", t0.Add(time.Hour), Policy{TTL: time.Minute, LockWait: 100 * time.Millisecond}, func() ([]byte, error) {
		t.Error("must not fetch while another process refreshes")
		return nil, nil
	})
	if !r.Stale || !errors.Is(r.Err, ErrBusy) || string(r.Body) != `{"v":1}` {
		t.Errorf("result = %+v", r)
	}
}
