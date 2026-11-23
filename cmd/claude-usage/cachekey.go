package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/Arwalk/wtf-claude/internal/api"
)

// cacheSuffix distinguishes cache files per account and server. The default
// config dir on the default server gets no suffix ("usage.json"); other
// config dirs and servers, and explicitly supplied tokens, get a short
// non-reversible hash so they never show each other's data.
func cacheSuffix(configDir string, explicitToken bool, token, baseURL string) string {
	key := ""
	switch {
	case explicitToken:
		key = "token:" + token
	default:
		if home, err := os.UserHomeDir(); err == nil && filepath.Clean(configDir) == filepath.Join(home, ".claude") {
			break
		}
		key = "dir:" + filepath.Clean(configDir)
	}
	if base := strings.TrimRight(baseURL, "/"); base != api.DefaultBaseURL {
		key += "\nbase:" + base
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "-" + hex.EncodeToString(sum[:4])
}
