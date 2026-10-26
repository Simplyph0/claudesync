//go:build !windows

package claude

import (
	"os/exec"
	"syscall"
)

func hideWindow(c *exec.Cmd) {}

// detach starts the app in its own session so it outlives ClaudeSync.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
