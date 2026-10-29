package projsync

import (
	"fmt"
	"strings"
	"time"
)

// Event is one line of a project's activity log.
type Event struct {
	Time  time.Time `json:"t"`
	Level string    `json:"l"` // info, warn, error
	Msg   string    `json:"m"`
	N     int       `json:"n,omitempty"` // how many more times it happened
}

const maxEvents = 60

// Result is what the last finished sync did.
type Result struct {
	Time       time.Time `json:"time"`
	Files      int       `json:"files"` // files in the folder
	Bytes      int64     `json:"bytes"`
	Uploaded   int       `json:"uploaded"`
	Downloaded int       `json:"downloaded"`
	Removed    int       `json:"removed"` // moved to the trash here
	Merged     int       `json:"merged"`
	Skipped    int       `json:"skipped"`
}

// Changed reports whether the sync changed anything.
func (r Result) Changed() bool { return r.Uploaded+r.Downloaded+r.Removed+r.Merged > 0 }

// Summary says what happened in plain words: "3 sent, 1 received".
func (r Result) Summary() string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(r.Uploaded, "sent to the server")
	add(r.Downloaded, "received")
	add(r.Merged, "merged")
	add(r.Removed, "moved to the trash")
	if len(parts) == 0 {
		return "nothing changed"
	}
	return strings.Join(parts, ", ")
}

// eventLocked adds a line to the project's log. The caller holds m.mu.
func (m *Manager) eventLocked(key, level, format string, args ...any) {
	st := m.stateLocked(key)
	msg := fmt.Sprintf(format, args...)
	if n := len(st.Events); n > 0 && st.Events[n-1].Level == level && st.Events[n-1].Msg == msg {
		st.Events[n-1].Time = m.Now() // the same thing again: count it, don't repeat it
		st.Events[n-1].N++
		return
	}
	st.Events = append(st.Events, Event{Time: m.Now(), Level: level, Msg: msg})
	if len(st.Events) > maxEvents {
		st.Events = append([]Event(nil), st.Events[len(st.Events)-maxEvents:]...)
	}
}

// event adds a line to the project's own activity log only.
func (m *Manager) event(key, level, format string, args ...any) {
	m.mu.Lock()
	m.eventLocked(key, level, format, args...)
	_ = m.saveStateLocked(key)
	m.mu.Unlock()
}

// note logs to the general log and the project's own activity log.
func (m *Manager) note(key, level, format string, args ...any) {
	m.mu.Lock()
	name := key
	if p := m.projects[key]; p != nil {
		name = p.Name
	}
	m.eventLocked(key, level, format, args...)
	_ = m.saveStateLocked(key)
	m.mu.Unlock()
	m.logf("project %s: "+format, append([]any{name}, args...)...)
}

// setProgress shows what a running sync is doing.
func (m *Manager) setProgress(key, format string, args ...any) {
	m.mu.Lock()
	if m.progress == nil {
		m.progress = map[string]string{}
	}
	m.progress[key] = fmt.Sprintf(format, args...)
	m.mu.Unlock()
}

// begin marks a project as syncing and starts a fresh result.
func (m *Manager) begin(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active, m.cur, m.progress = map[string]bool{}, map[string]*Result{}, map[string]string{}
	}
	m.active[key] = true
	m.cur[key] = &Result{}
	m.progress[key] = "Checking the folder…"
}

func (m *Manager) end(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, key)
	delete(m.cur, key)
	delete(m.progress, key)
}

// res is the result being collected for a running sync (a throwaway one if
// none is running, so callers needn't check).
func (m *Manager) res(key string) *Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.cur[key]; r != nil {
		return r
	}
	return &Result{}
}

// countFiles counts files (not folders) and their size.
func countFiles(files map[string]*Entry) (n int, bytes int64) {
	for _, e := range files {
		if !e.Dir {
			n++
			bytes += e.Size
		}
	}
	return
}

// changedFiles counts files in now that are new or different from before.
func changedFiles(before, now map[string]*Entry) int {
	n := 0
	for k, e := range now {
		if e.Dir {
			continue
		}
		if b := before[k]; b == nil || !same(b, e) {
			n++
		}
	}
	return n
}
