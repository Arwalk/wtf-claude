package main

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/Arwalk/wtf-claude/internal/api"
	"github.com/Arwalk/wtf-claude/internal/cache"
)

var profileEntry = regexp.MustCompile(`^profile(-[0-9a-f]{8})?$`)

// scrubProfiles trims profile entries written by versions that cached the
// whole response, which includes the email and names of the account holder.
// All accounts are covered: an entry nobody refreshes any more would
// otherwise keep them forever.
func scrubProfiles(store cache.Store) {
	// Not filepath.Glob: the directory's path may hold pattern characters.
	files, _ := os.ReadDir(store.Dir)
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || !profileEntry.MatchString(name) || trimmed(store.Load(name)) == nil {
			continue
		}
		// Take the refresh lock and look again, so that an entry another
		// process is writing right now is not replaced by an older one.
		unlock, err := store.Lock(name, 0)
		if errors.Is(err, cache.ErrLockTimeout) {
			continue // that refresh, or the next run, trims it
		}
		e := store.Load(name)
		if body := trimmed(e); body != nil {
			e.Body = body
			_ = store.Save(name, e)
		}
		if err == nil {
			unlock()
		}
	}
}

// trimmed returns the body e should have, or nil if there is nothing to trim.
func trimmed(e cache.Entry) []byte {
	if !e.HasBody() {
		return nil
	}
	body, err := api.TrimProfile(e.Body)
	if err != nil || bytes.Equal(body, e.Body) {
		return nil
	}
	return body
}
