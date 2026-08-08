//go:build windows

package simplerouter

import "golang.org/x/sys/windows"

// enableTerminalVT turns on virtual-terminal input on the console input handle
// (so arrow keys arrive as ANSI escape sequences via ReadFile rather than being
// swallowed) and virtual-terminal processing on the output handle (so our color
// and cursor-movement escapes are interpreted). term.Restore later clears the
// input bit when it restores the original console mode.
func enableTerminalVT(inFd, outFd uintptr) {
	if mode, err := getConsoleMode(inFd); err == nil {
		windows.SetConsoleMode(windows.Handle(inFd), mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	}
	if mode, err := getConsoleMode(outFd); err == nil {
		windows.SetConsoleMode(windows.Handle(outFd), mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}

func getConsoleMode(fd uintptr) (uint32, error) {
	var mode uint32
	err := windows.GetConsoleMode(windows.Handle(fd), &mode)
	return mode, err
}
