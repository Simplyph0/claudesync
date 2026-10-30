package projsync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func chatWorld(t *testing.T) (*Manager, func(name string, manual bool) Project, *time.Time) {
	root := t.TempDir()
	m, err := Open(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	m.Now = func() time.Time { return clock }
	add := func(name string, manual bool) Project {
		dir := filepath.Join(root, "home", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o644)
		p := Project{Key: "name:" + name, Name: name, Folder: dir, Mode: ModeSync, Manual: manual}
		if err := m.SetProject(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return m, add, &clock
}

// A folder whose chats are all deleted is dropped from the list, but only
// after it stayed chat-less for a while, and its files are never touched.
func TestFolderGoesWhenItsLastChatIsDeleted(t *testing.T) {
	m, add, clock := chatWorld(t)
	p := add("sketch", false)
	grace := 5 * time.Minute

	if gone := m.ReconcileChats([]string{p.Folder}, true, true, grace); len(gone) != 0 {
		t.Fatalf("removed while it has a chat: %v", gone)
	}
	if got, _ := m.Get(p.Key); !got.ChatSeen {
		t.Fatal("didn't note that a chat used it")
	}
	// the chat is deleted: noted now, removed only once it has stayed that way
	if gone := m.ReconcileChats(nil, true, true, grace); len(gone) != 0 {
		t.Fatalf("removed at the first look: %v", gone)
	}
	*clock = clock.Add(2 * time.Minute)
	if gone := m.ReconcileChats(nil, true, true, grace); len(gone) != 0 {
		t.Fatalf("removed before the grace time: %v", gone)
	}
	*clock = clock.Add(4 * time.Minute)
	gone := m.ReconcileChats(nil, true, true, grace)
	if len(gone) != 1 || gone[0].Key != p.Key {
		t.Fatalf("not removed: %v", gone)
	}
	if _, ok := m.Get(p.Key); ok {
		t.Fatal("still in the list")
	}
	if b, err := os.ReadFile(filepath.Join(p.Folder, "keep.txt")); err != nil || string(b) != "mine" {
		t.Fatalf("its files were touched: %q %v", b, err)
	}
	m2, _ := Open(m.Dir) // and it stays gone after a restart
	if _, ok := m2.Get(p.Key); ok {
		t.Fatal("came back after a restart")
	}
}

func TestChatReturningBeforeTheGraceTimeKeepsIt(t *testing.T) {
	m, add, clock := chatWorld(t)
	p := add("sketch", false)
	m.ReconcileChats([]string{p.Folder}, true, true, 5*time.Minute)
	m.ReconcileChats(nil, true, true, 5*time.Minute)
	*clock = clock.Add(3 * time.Minute)
	m.ReconcileChats([]string{p.Folder}, true, true, 5*time.Minute) // a new chat in it
	*clock = clock.Add(10 * time.Minute)
	if gone := m.ReconcileChats([]string{p.Folder}, true, true, 5*time.Minute); len(gone) != 0 {
		t.Fatalf("removed: %v", gone)
	}
	if got, _ := m.Get(p.Key); !got.NoChatsSince.IsZero() {
		t.Fatal("the countdown wasn't cleared")
	}
}

// Things that must never be removed.
func TestFoldersThatStay(t *testing.T) {
	m, add, clock := chatWorld(t)
	manual := add("handadded", true)
	never := add("nochatever", false)
	asking := add("asking", false)
	sub := add("monorepo", false)
	for _, p := range []Project{manual, asking, sub} {
		m.ReconcileChats([]string{p.Folder}, true, true, time.Minute)
	}
	m.mu.Lock()
	m.stateLocked(asking.Key).Decision = &Decision{Kind: "git", Message: "?"}
	m.mu.Unlock()
	// chats in a subfolder count for the folder above it
	subChat := filepath.Join(sub.Folder, "packages", "app")

	for i := 0; i < 3; i++ {
		*clock = clock.Add(10 * time.Minute)
		if gone := m.ReconcileChats([]string{subChat}, true, true, time.Minute); len(gone) != 0 {
			t.Fatalf("removed %v", gone)
		}
	}
	for _, p := range []Project{manual, never, asking, sub} {
		if _, ok := m.Get(p.Key); !ok {
			t.Errorf("%s was removed", p.Name)
		}
	}

	// Not a trustworthy look (the sessions couldn't be read), or the setting is off.
	one := add("onechat", false)
	m.ReconcileChats([]string{one.Folder}, true, true, time.Minute)
	for _, c := range []struct{ healthy, prune bool }{{false, true}, {true, false}} {
		for i := 0; i < 3; i++ {
			*clock = clock.Add(10 * time.Minute)
			if gone := m.ReconcileChats(nil, c.healthy, c.prune, time.Minute); len(gone) != 0 {
				t.Fatalf("%+v removed %v", c, gone)
			}
		}
	}
	// ...and neither leaves a half-run countdown behind that removes it the moment it's allowed.
	if gone := m.ReconcileChats(nil, true, true, time.Minute); len(gone) != 0 {
		t.Fatalf("removed straight away: %v", gone)
	}
}
