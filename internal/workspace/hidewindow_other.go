//go:build !windows

package workspace

import "os/exec"

func hideWindow(*exec.Cmd) {}
