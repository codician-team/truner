//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on ANSI escape processing in Windows consoles.
func enableVT() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
	}
}
