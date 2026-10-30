package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperInstance is not a test: run as a child process, it stands in for
// a running ClaudeSync. It listens on CS_TEST_ADDR and, if CS_TEST_OBEY is
// set, exits when told "quit" (as new versions do); otherwise it ignores it
// (as versions before 0.5.2 did).
func TestHelperInstance(t *testing.T) {
	addr := os.Getenv("CS_TEST_ADDR")
	if addr == "" {
		t.Skip("only used as a child process")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if os.Getenv("CS_TEST_OBEY") == "1" && strings.TrimSpace(line) == "quit" {
			os.Exit(0)
		}
	}
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startInstance runs the helper from the binary at bin and waits until it listens.
func startInstance(t *testing.T, bin, addr string, obey bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "-test.run=^TestHelperInstance$")
	cmd.Env = append(os.Environ(), "CS_TEST_ADDR="+addr, "CS_TEST_OBEY="+map[bool]string{true: "1", false: "0"}[obey])
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "ready" {
				ready <- true
				io.Copy(io.Discard, out)
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper process didn't start")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("helper process never became ready")
	}
	return cmd
}

func exited(cmd *exec.Cmd) bool {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func TestStopRunningNothingRunning(t *testing.T) {
	if err := stopRunning(freeAddr(t)); err != nil {
		t.Fatal(err)
	}
}

// A copy that understands "quit" is asked nicely, and not killed.
func TestStopRunningAsksNicely(t *testing.T) {
	addr := freeAddr(t)
	cmd := startInstance(t, os.Args[0], addr, true)
	if err := stopRunning(addr); err != nil {
		t.Fatal(err)
	}
	if !exited(cmd) {
		t.Fatal("the running copy didn't quit")
	}
}

// The bug that left updates unapplied: an older copy ignores "quit" and keeps
// the port, so the new version never got to run. It's stopped by force.
func TestStopRunningOldVersionThatIgnoresQuit(t *testing.T) {
	old := quitWait
	quitWait = 700 * time.Millisecond
	defer func() { quitWait = old }()
	addr := freeAddr(t)
	cmd := startInstance(t, os.Args[0], addr, false)
	if err := stopRunning(addr); err != nil {
		t.Fatal(err)
	}
	if !exited(cmd) {
		t.Fatal("the old copy is still running")
	}
	if portHeld(addr) {
		t.Fatal("the port wasn't released")
	}
}

// Something else on the port is never touched.
func TestStopRunningLeavesOtherProgramsAlone(t *testing.T) {
	old := quitWait
	quitWait = 500 * time.Millisecond
	defer func() { quitWait = old }()
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "otherprogram")
	if err := os.WriteFile(other, src, 0o755); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	cmd := startInstance(t, other, addr, false)
	err = stopRunning(addr)
	if err == nil || !strings.Contains(err.Error(), "isn't ClaudeSync") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if cmd.ProcessState != nil || !portHeld(addr) {
		t.Fatal("the other program was disturbed")
	}
}

func TestInstanceCommands(t *testing.T) {
	var shown, quit int
	for line, want := range map[string][2]int{"show\n": {1, 0}, "quit\n": {0, 1}, "quit": {0, 1}, "\n": {0, 0}, "rm -rf\n": {0, 0}} {
		shown, quit = 0, 0
		handleInstanceCommand(line, func() { shown++ }, func() { quit++ })
		if shown != want[0] || quit != want[1] {
			t.Errorf("%q: shown %d, quit %d", line, shown, quit)
		}
	}
}
