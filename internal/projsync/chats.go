package projsync

import (
	"os"
	"time"
)

// hasChat reports whether any chat folder is the project folder or inside it.
func hasChat(folder string, chatDirs []string) bool {
	for _, d := range chatDirs {
		if samePath(d, folder) || within(d, folder) {
			return true
		}
	}
	return false
}

// ReconcileChats keeps track of which project folders have chats on this PC,
// and, when prune is set, forgets a folder once its chats are all gone (every
// chat that used it was deleted). Forgetting only removes it from this PC's
// list: its files and the copy on the server are never touched.
//
// It is careful, because a wrong "no chats" would drop real projects:
//   - healthy must be true: the sessions were actually read in this run.
//   - the folder must have had a chat before (ChatSeen), and not have been
//     added by hand (Manual).
//   - nothing waiting on the user (a question, a clash) and nothing syncing.
//   - it has to stay chat-less for grace, across at least two checks.
//
// It returns the projects it forgot.
func (m *Manager) ReconcileChats(chatDirs []string, healthy, prune bool, grace time.Duration) []Project {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	changed := false
	var gone []Project
	for key, p := range m.projects {
		if hasChat(p.Folder, chatDirs) {
			if !p.ChatSeen || !p.NoChatsSince.IsZero() {
				p.ChatSeen, p.NoChatsSince = true, time.Time{}
				changed = true
			}
			continue
		}
		if !healthy || !prune || !p.ChatSeen || p.Manual {
			if !p.NoChatsSince.IsZero() {
				p.NoChatsSince = time.Time{}
				changed = true
			}
			continue
		}
		if st := m.stateLocked(key); st.Decision != nil || len(st.Clashes) > 0 || m.active[key] {
			continue
		}
		if p.NoChatsSince.IsZero() {
			p.NoChatsSince = now
			changed = true
			continue
		}
		if now.Sub(p.NoChatsSince) >= grace {
			gone = append(gone, *p)
		}
	}
	for _, p := range gone {
		delete(m.projects, p.Key)
		delete(m.states, p.Key)
		_ = os.Remove(m.basePath(p.Key))
		_ = os.Remove(m.statePath(p.Key))
	}
	if changed || len(gone) > 0 {
		_ = m.saveProjectsLocked()
	}
	return gone
}
