//go:build linux

package claude

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// These tests only ever start and stop their own stand-in programs, which live
// in a temporary folder. A real Claude running on the machine is never touched.

// fakeApp starts a stand-in for Claude: a program called claude-desktop with
// helper processes under it. With ignoreTerm its main process ignores SIGTERM,
// like a Claude stuck half-way through closing.
func fakeApp(t *testing.T, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sh, _ = filepath.EvalSymlinks(sh)
	dst := filepath.Join(t.TempDir(), "claude-desktop")
	in, err := os.Open(sh)
	if err != nil {
		t.Skip(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
	script := `sleep 300 & sleep 300 & wait`
	if ignoreTerm {
		script = `trap "" TERM; ` + script
	}
	c := exec.Command(dst, "-c", script)
	c.Env = os.Environ()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func running(pid int) bool {
	if ok, _ := process.PidExists(int32(pid)); !ok {
		return false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	st, _ := p.Status()
	return !(len(st) > 0 && st[0] == "Z")
}

// mainOf builds the App for the stand-in the way RunningApps would report it.
func mainOf(t *testing.T, c *exec.Cmd) []App {
	t.Helper()
	var apps []App
	waitFor(t, "the stand-in to start", func() bool {
		p, err := process.NewProcess(int32(c.Process.Pid))
		if err != nil {
			return false
		}
		exe, err := p.Exe()
		if err != nil {
			return false
		}
		cmd, _ := p.CmdlineSlice()
		if !IsDesktopProcess(exe, cmd) || hasTypeFlag(cmd) {
			t.Fatalf("the stand-in isn't recognised as Claude: %s %v", exe, cmd)
		}
		apps = []App{{PID: p.Pid, Exe: exe, Cmdline: cmd}}
		return true
	})
	return apps
}

// A Claude that doesn't stop when asked (it hangs half-way, leaving its icon
// and helpers) is stopped for good, helpers included, and nothing else is touched.
func TestQuitStopsAHungApp(t *testing.T) {
	politeWait = 700 * time.Millisecond
	t.Cleanup(func() { politeWait = 8 * time.Second })
	app := fakeApp(t, true)
	bystander := exec.Command("sleep", "300") // not part of the app: must survive
	if err := bystander.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	mains := mainOf(t, app)

	var kids []int
	waitFor(t, "helpers", func() bool {
		kids = nil
		for _, p := range belongs(mains) {
			if int(p.Pid) != app.Process.Pid {
				kids = append(kids, int(p.Pid))
			}
		}
		return len(kids) >= 2
	})
	for _, p := range belongs(mains) {
		if int(p.Pid) == bystander.Process.Pid {
			t.Fatal("an unrelated process was counted as part of the app")
		}
	}
	go app.Wait() // reap it, as init would

	if err := Quit(context.Background(), mains, t.Logf); err != nil {
		t.Fatal(err)
	}
	if running(app.Process.Pid) {
		t.Fatal("the main process is still running")
	}
	for _, k := range kids {
		if running(k) {
			t.Fatalf("helper %d is still running", k)
		}
	}
	if !running(bystander.Process.Pid) {
		t.Fatal("an unrelated process was stopped")
	}
}

// A Claude that does stop when asked is closed without being killed.
func TestQuitIsPoliteFirst(t *testing.T) {
	app := fakeApp(t, false)
	mains := mainOf(t, app)
	go app.Wait()
	start := time.Now()
	if err := Quit(context.Background(), mains, t.Logf); err != nil {
		t.Fatal(err)
	}
	if running(app.Process.Pid) {
		t.Fatal("still running")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v for an app that stops when asked", time.Since(start))
	}
}

func TestClearStaleLocks(t *testing.T) {
	dir := t.TempDir()
	lock := func(target string) {
		for _, n := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
			os.Remove(filepath.Join(dir, n))
		}
		os.Symlink(target, filepath.Join(dir, "SingletonLock"))
		os.Symlink("123456", filepath.Join(dir, "SingletonCookie"))
		os.Symlink("/tmp/x/SingletonSocket", filepath.Join(dir, "SingletonSocket"))
	}
	lock("myhost-" + strconv.Itoa(os.Getpid())) // held by a live process
	ClearStaleLocks(dir)
	if _, err := os.Readlink(filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal("removed a lock whose process is alive")
	}
	lock("myhost-4194301") // nobody home
	ClearStaleLocks(dir)
	for _, n := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s left behind", n)
		}
	}
	ClearStaleLocks(filepath.Join(dir, "nothing")) // no folder: no problem
}
