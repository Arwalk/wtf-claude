package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// cacheSuffix distinguishes cache files per account. The default config dir
// gets no suffix ("usage.json"); other config dirs, and explicitly supplied
// tokens, get a short non-reversible hash so separate accounts never show
// each other's data.
func cacheSuffix(configDir string, explicitToken bool, token string) string {
	key := ""
	switch {
	case explicitToken:
		key = "token:" + token
	default:
		if home, err := os.UserHomeDir(); err == nil && filepath.Clean(configDir) == filepath.Join(home, ".claude") {
			return ""
		}
		key = "dir:" + filepath.Clean(configDir)
	}
	sum := sha256.Sum256([]byte(key))
	return "-" + hex.EncodeToString(sum[:4])
}
