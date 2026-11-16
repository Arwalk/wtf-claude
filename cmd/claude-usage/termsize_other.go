//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "os"

// terminalWidth is not implemented on this platform; callers fall back to
// the default width.
func terminalWidth(*os.File) int { return 0 }
