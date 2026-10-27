package claude

import (
	"context"
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

// ErrStillRunning means Claude didn't quit when asked. ClaudeSync never
// force-closes it; the user has to quit it themselves.
var ErrStillRunning = errors.New("Claude is still open")

// App is a running desktop app instance (its main process).
type App struct {
	PID     int32
	Exe     string
	Cmdline []string
	Env     []string
}

// IsDesktopProcess decides whether a process belongs to the Claude desktop
// app, and not to the Claude Code CLI, an editor or a shell that merely
// mentions a Claude folder.
func IsDesktopProcess(exe string, cmdline []string) bool {
	e := strings.ToLower(filepath.ToSlash(exe))
	base := filepath.Base(e)
	if strings.Contains(e, "claude-code") || strings.Contains(e, "/node_modules/.bin/") {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return base == "claude.exe" && (strings.Contains(e, "/windowsapps/claude_") || strings.Contains(e, "/anthropicclaude/"))
	case "darwin":
		return strings.Contains(e, "/claude.app/contents/")
	default:
		switch base {
		case "electron", "claude-desktop", "claude", "claude.appimage":
		default:
			return false
		}
		if strings.HasSuffix(e, "/.local/bin/claude") || strings.Contains(e, "/.claude/") {
			return false // the Claude Code CLI
		}
		all := e + " " + strings.ToLower(strings.Join(cmdline, " "))
		return strings.Contains(all, "claude-desktop") || strings.Contains(all, "claude.appimage") || strings.Contains(all, "/.mount_claude")
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
		env, _ := p.Environ()
		out = append(out, App{PID: p.Pid, Exe: exe, Cmdline: cmd, Env: env})
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

// DesktopRunning reports whether the desktop app is open. If processes can't
// be listed it says yes, so nothing is written under a running app.
func DesktopRunning() bool {
	apps, err := RunningApps()
	return err != nil || len(apps) > 0
}

// AskToQuit asks each app to close the normal way. It never kills anything.
// On Windows this is the same as clicking the window's ✕, which Claude may
// answer by hiding in the tray, so callers must not assume it worked.
func AskToQuit(apps []App) {
	for _, a := range apps {
		askToQuit(a.PID)
	}
}

// WaitClosed waits up to d for the app to exit, then a moment more for it to
// finish writing its files. It reports whether it closed.
func WaitClosed(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if !DesktopRunning() {
			time.Sleep(2 * time.Second)
			return !DesktopRunning()
		}
		time.Sleep(time.Second)
	}
	return false
}

func askToQuit(pid int32) {
	if runtime.GOOS == "windows" {
		c := exec.Command("taskkill", "/PID", strconv.Itoa(int(pid))) // no /F: sends a close request
		hideWindow(c)
		_ = c.Run()
		return
	}
	if p, err := process.NewProcess(pid); err == nil {
		_ = p.Terminate() // SIGTERM: Electron quits cleanly
	}
}

// Relaunch opens the app again through its own launcher. It never replays a
// raw Electron command line, which can start the app with the wrong profile.
func Relaunch(apps []App) error {
	if len(apps) == 0 {
		return nil
	}
	a := apps[0]
	switch runtime.GOOS {
	case "windows":
		if aumid := storeAppID(a.Exe); aumid != "" {
			return exec.Command("explorer.exe", `shell:AppsFolder\`+aumid).Start()
		}
		return exec.Command(a.Exe).Start()
	case "darwin":
		return exec.Command("open", "-a", "Claude").Start()
	}
	for _, kv := range a.Env {
		if p, ok := strings.CutPrefix(kv, "APPIMAGE="); ok && p != "" {
			return startDetached(p)
		}
	}
	if p, err := exec.LookPath("claude-desktop"); err == nil {
		return startDetached(p)
	}
	return errors.New("no Claude launcher found")
}

func startDetached(path string) error {
	c := exec.Command(path)
	c.Env = os.Environ()
	detach(c)
	return c.Start()
}

// storeAppID turns ...\WindowsApps\Claude_3.1.0.0_x64__pzs8sxrjxfjjc\app\Claude.exe
// into the Store app id "Claude_pzs8sxrjxfjjc!Claude".
func storeAppID(exe string) string {
	parts := strings.Split(strings.ReplaceAll(exe, `\`, "/"), "/")
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
