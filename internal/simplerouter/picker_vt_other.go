//go:build !windows

package simplerouter

// enableTerminalVT is a no-op outside Windows: term.MakeRaw already delivers
// raw bytes including arrow-key escape sequences.
func enableTerminalVT(inFd, outFd uintptr) {}
