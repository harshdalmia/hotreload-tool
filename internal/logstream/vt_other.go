//go:build !windows

package logstream

import "os"

// enableVirtualTerminal is a no-op away from Windows: terminals on Unix
// interpret ANSI sequences without being asked, and the caller has already
// confirmed the handle is a character device.
func enableVirtualTerminal(_ *os.File) bool {
	return true
}
