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

// belongs lists the processes of the given app instances: the main process,
// whatever it started (helpers, the Claude Code sessions, the Cowork helper),
// and helpers of the same program that lost their parent. It is exact on
// purpose: only the instances passed in are touched, never another Claude, our
// own process, or a Claude Code CLI you started yourself.
func belongs(mains []App) []*process.Process {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	self := int32(os.Getpid())
	parent := map[int32]int32{}
	byPID := map[int32]*process.Process{}
	for _, p := range procs {
		byPID[p.Pid] = p
		if pp, err := p.Ppid(); err == nil {
			parent[p.Pid] = pp
		}
	}
	mine := map[int32]bool{}
	for _, a := range mains {
		mine[a.PID] = true
	}
	for changed := true; changed; { // everything below the main processes
		changed = false
		for pid, pp := range parent {
			if !mine[pid] && mine[pp] && pid != self {
				mine[pid], changed = true, true
			}
		}
	}
	programs := map[string]bool{}
	for _, a := range mains {
		programs[a.Exe] = true
	}
	for _, p := range procs { // helpers (--type=…) of the same program file that lost their parent
		if p.Pid == self || mine[p.Pid] {
			continue
		}
		if exe, err := p.Exe(); err == nil && programs[exe] {
			if cmd, _ := p.CmdlineSlice(); hasTypeFlag(cmd) {
				mine[p.Pid] = true
			}
		}
	}
	var out []*process.Process
	for pid := range mine {
		if p := byPID[pid]; p != nil && pid != self {
			out = append(out, p)
		}
	}
	return out
}

// anyAlive reports whether any of the app's processes is still there.
func anyAlive(mains []App) bool {
	for _, p := range belongs(mains) {
		if st, err := p.Status(); err == nil && len(st) > 0 && st[0] == "Z" {
			continue // a zombie is gone as far as we care
		}
		return true
	}
	return false
}

func waitGone(ctx context.Context, mains []App, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !anyAlive(mains) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// politeWait is how long Quit lets the app stop by itself before killing it.
var politeWait = 8 * time.Second

// Quit closes the desktop app and everything it started, and only returns
// once it has really gone. Closing the window, or asking nicely, is not
// enough: Claude can hide in the tray, or stop half-way and leave its tray
// icon and its helpers behind, which blocks it from being opened again.
//
//  1. Ask every process of the app to stop (SIGTERM on Linux, a close request
//     on Windows) and give them a few seconds.
//  2. Anything left is killed.
//
// Chats running in Claude stop. Everything ClaudeSync restores is saved to a
// snapshot first, and this PC's own chats are backed up before every sync.
func Quit(ctx context.Context, mains []App, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(mains) == 0 {
		return nil
	}
	logf("closing Claude: asking it to stop")
	stopAll(mains, false)
	if waitGone(ctx, mains, politeWait) {
		time.Sleep(time.Second) // a moment for its last file writes
		return nil
	}
	logf("Claude didn't close by itself; stopping what's left of it")
	stopAll(mains, true)
	if waitGone(ctx, mains, 6*time.Second) {
		time.Sleep(time.Second)
		return nil
	}
	return errors.New("Claude is still running after being stopped; quit it yourself from its tray icon")
}

// stopAll asks (or, with force, makes) every process of the app stop.
func stopAll(mains []App, force bool) {
	if runtime.GOOS == "windows" {
		for _, a := range mains {
			args := []string{"/T", "/PID", strconv.Itoa(int(a.PID))}
			if force {
				args = append([]string{"/F"}, args...)
			}
			c := exec.Command("taskkill", args...)
			hideWindow(c)
			_ = c.Run()
		}
		if force { // helpers that lost their parent
			for _, p := range belongs(mains) {
				_ = p.Kill()
			}
		}
		return
	}
	for _, p := range belongs(mains) {
		if force {
			_ = p.Kill() // SIGKILL
		} else {
			_ = p.Terminate() // SIGTERM, like pkill
		}
	}
}

// WaitRunning waits up to d for the desktop app to be running.
func WaitRunning(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if apps, err := RunningApps(); err == nil && len(apps) > 0 {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ClearStaleLocks removes the single-instance lock a stopped Claude can leave
// behind (Electron apps on Linux). A lock that points at a process that still
// runs is left alone. Call it only when Claude isn't running.
func ClearStaleLocks(desktopDir string) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" || desktopDir == "" {
		return
	}
	lock := filepath.Join(desktopDir, "SingletonLock")
	target, err := os.Readlink(lock)
	if err != nil {
		return // no lock
	}
	if i := strings.LastIndex(target, "-"); i >= 0 {
		if pid, err := strconv.Atoi(target[i+1:]); err == nil {
			if ok, _ := process.PidExists(int32(pid)); ok {
				return // that instance is alive
			}
		}
	}
	for _, n := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(desktopDir, n))
	}
}
