package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// logBuf keeps recent log lines for the Log tab and appends them to a file.
type logBuf struct {
	mu       sync.Mutex
	lines    []string
	file     *os.File
	onChange func()
}

func newLogBuf(dir string) *logBuf {
	l := &logBuf{}
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "claudesync.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		l.file = f
	}
	return l
}

func (l *logBuf) Printf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + "  " + fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > 500 {
		l.lines = l.lines[len(l.lines)-500:]
	}
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	}
	cb := l.onChange
	l.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (l *logBuf) Text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
