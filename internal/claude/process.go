package claude

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// App is a running desktop app instance (its main process).
type App struct {
	PID     int32
	Exe     string
	Cmdline []string
}

// IsDesktopProcess decides whether a process belongs to the Claude desktop
// app (not the Claude Code CLI, which is also called claude.exe on Windows).
func IsDesktopProcess(exe string, cmdline []string) bool {
	e := strings.ToLower(filepath.ToSlash(exe))
	if strings.Contains(e, "claude-code") || strings.Contains(e, "/.local/bin/claude") || strings.Contains(e, "/node_modules/") {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		base := filepath.Base(e)
		return base == "claude.exe" && (strings.Contains(e, "/windowsapps/claude_") || strings.Contains(e, "/anthropicclaude/"))
	case "darwin":
		return strings.Contains(e, "/claude.app/")
	default:
		all := e + " " + strings.ToLower(strings.Join(cmdline, " "))
		return strings.Contains(all, "claude-desktop") || strings.Contains(all, "claude.appimage") ||
			strings.Contains(all, "/.mount_claude")
	}
}

// RunningApps returns the desktop app's main processes (Electron helper
// processes, which carry a --type= flag, are skipped).
func RunningApps() ([]App, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	var out []App
	for _, p := range procs {
		exe, err := p.Exe()
		if err != nil || exe == "" {
			continue
		}
		cmd, _ := p.CmdlineSlice()
		if !IsDesktopProcess(exe, cmd) || hasTypeFlag(cmd) {
			continue
		}
		out = append(out, App{PID: p.Pid, Exe: exe, Cmdline: cmd})
	}
	return out, nil
}

func hasTypeFlag(cmd []string) bool {
	for _, a := range cmd {
		if strings.HasPrefix(a, "--type=") {
			return true
		}
	}
	return false
}

// DesktopRunning reports whether the desktop app is open.
func DesktopRunning() bool {
	apps, err := RunningApps()
	return err == nil && len(apps) > 0
}

// CloseDesktop asks the app to quit, waits, then force-closes it. It returns
// what was running so Relaunch can start it again.
func CloseDesktop(wait time.Duration) ([]App, error) {
	apps, err := RunningApps()
	if err != nil || len(apps) == 0 {
		return apps, err
	}
	for _, a := range apps {
		gracefulStop(a.PID)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !DesktopRunning() {
			return apps, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Still open (e.g. minimised to the tray): force it, including helpers.
	procs, _ := process.Processes()
	for _, p := range procs {
		exe, _ := p.Exe()
		cmd, _ := p.CmdlineSlice()
		if exe != "" && IsDesktopProcess(exe, cmd) {
			_ = p.Kill()
		}
	}
	time.Sleep(time.Second)
	if DesktopRunning() {
		return apps, errors.New("could not close the Claude app")
	}
	return apps, nil
}

func gracefulStop(pid int32) {
	if runtime.GOOS == "windows" {
		c := exec.Command("taskkill", "/PID", itoa(pid))
		hideWindow(c)
		_ = c.Run()
		return
	}
	if p, err := os.FindProcess(int(pid)); err == nil {
		_ = p.Signal(os.Interrupt)
		if p2, err := process.NewProcess(pid); err == nil {
			_ = p2.Terminate()
		}
	}
}

// Relaunch starts the app again the way it was running.
func Relaunch(apps []App) error {
	if len(apps) == 0 {
		return nil
	}
	a := apps[0]
	if runtime.GOOS == "windows" {
		if aumid := storeAppID(a.Exe); aumid != "" {
			c := exec.Command("explorer.exe", `shell:AppsFolder\`+aumid)
			return c.Start()
		}
		c := exec.Command(a.Exe)
		return c.Start()
	}
	args := a.Cmdline
	if len(args) == 0 {
		args = []string{a.Exe}
	}
	c := exec.Command(args[0], args[1:]...)
	detach(c)
	return c.Start()
}

// storeAppID turns ...\WindowsApps\Claude_3.1.0.0_x64__pzs8sxrjxfjjc\app\Claude.exe
// into the Store app id "Claude_pzs8sxrjxfjjc!Claude".
func storeAppID(exe string) string {
	parts := strings.Split(filepath.ToSlash(exe), "/")
	for i, p := range parts {
		if strings.EqualFold(p, "WindowsApps") && i+1 < len(parts) {
			pkg := parts[i+1]
			name, rest, ok := strings.Cut(pkg, "_")
			_, publisher, ok2 := strings.Cut(rest, "__")
			if ok && ok2 {
				return name + "_" + publisher + "!Claude"
			}
		}
	}
	return ""
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }
