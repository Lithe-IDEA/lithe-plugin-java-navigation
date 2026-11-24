//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package lithe

import (
	"io"
	"os"
	"syscall"
)

// protocolOutput is the stream the protocol is written to: a duplicate of
// the original stdout descriptor, with descriptor 1 pointed at stderr, so
// output printed by plugin code, its dependencies or the processes it starts
// cannot corrupt the protocol. The duplicate is close-on-exec, so child
// processes do not hold the protocol pipe open.
func protocolOutput() io.Writer {
	syscall.ForkLock.RLock()
	protocol, err := syscall.Dup(1)
	if err == nil {
		syscall.CloseOnExec(protocol)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return os.Stdout
	}
	if err := syscall.Dup2(2, 1); err != nil {
		os.Exit(1)
	}
	return os.NewFile(uintptr(protocol), "lithe-protocol")
}
