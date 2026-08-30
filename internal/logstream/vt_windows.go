//go:build windows

package logstream

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on ANSI escape sequence processing for a Windows
// console.
//
// Windows 10 and later can interpret ANSI codes, but not by default: a console
// starts without ENABLE_VIRTUAL_TERMINAL_PROCESSING, and anything written to it
// containing escape sequences is rendered literally, so a dimmed prefix would
// show up as visible junk instead. Older consoles and redirected handles fail
// the mode calls, and reporting false there correctly disables colour.
func enableVirtualTerminal(f *os.File) bool {
	handle := windows.Handle(f.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}
