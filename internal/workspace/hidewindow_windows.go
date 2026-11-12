package workspace

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps a console program started by this windowless server
// from opening a console window.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
