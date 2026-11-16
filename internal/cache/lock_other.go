//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package cache

import (
	"errors"
	"time"
)

// lock is unsupported on this platform; callers proceed without it.
func (s Store) lock(string, time.Duration) (func(), error) {
	return nil, errors.New("cache: locking not supported")
}
