package claude

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func detach(c *exec.Cmd) {}
