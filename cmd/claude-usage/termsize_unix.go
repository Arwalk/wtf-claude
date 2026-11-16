//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth returns the column count of the terminal f is attached to,
// or 0 if f is not a terminal or the size is unknown.
func terminalWidth(f *os.File) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
