//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package cache

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lock takes an exclusive advisory lock on <dir>/<name>.lock, waiting up to
// wait. It returns ErrLockTimeout if another process kept the lock, or
// another error if locking is not possible (callers then proceed unlocked).
func (s Store) lock(name string, wait time.Duration) (func(), error) {
	if s.Dir == "" {
		return nil, errors.New("cache: no directory")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, name+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}
