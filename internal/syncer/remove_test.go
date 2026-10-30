package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Simplyph0/claudesync/internal/proto"
)

func serverItem(t *testing.T, e *Engine, id string) *ServerItem {
	t.Helper()
	items, err := e.ServerItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if items[i].Header.ID == id {
			return &items[i]
		}
	}
	return nil
}

// Removing from the server backup stops syncing everywhere, never deletes
// anyone's local copy, isn't undone by the next backup, and can be reversed.
func TestRemoveFromServer(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Keep")
	desk.addSession("local_bbbb", cliB, "Strike me")
	desk.sync()

	it := serverItem(t, desk.eng, "local_bbbb")
	if it == nil || it.Title != "Strike me" {
		t.Fatalf("server list should show the encrypted title: %+v", it)
	}
	if err := desk.eng.RemoveFromServer(context.Background(), proto.KindSession, "local_bbbb"); err != nil {
		t.Fatal(err)
	}

	lap := w.pc("Laptop", "code")
	lap.sync()
	if s := lap.sessions(); len(s) != 1 || s[0] != "Keep" {
		t.Fatalf("laptop restored a removed item: %v", s)
	}
	// The desktop still has its copy, keeps working on it, and doesn't re-upload it.
	desk.appendLine(cliB, "still here")
	desk.sync()
	if s := desk.sessions(); len(s) != 2 {
		t.Fatalf("removing deleted the desktop's copy: %v", s)
	}
	if it := serverItem(t, desk.eng, "local_bbbb"); it == nil || !it.Header.Removed || it.Header.Version != 1 {
		t.Fatalf("removed item was uploaded again: %+v", it)
	}

	// Bringing it back resumes syncing: the desktop's newer copy uploads and the laptop gets it.
	if err := lap.eng.BringBack(context.Background(), proto.KindSession, "local_bbbb"); err != nil {
		t.Fatal(err)
	}
	desk.sync()
	lap.sync()
	if s := lap.sessions(); len(s) != 2 || !has(s, "Strike me") {
		t.Fatalf("brought-back item didn't sync: %v", s)
	}
}

// Skipping takes an item off this PC's waiting list for good, without
// affecting the server or other PCs; un-skipping restores it.
func TestSkipPending(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Wanted")
	desk.addSession("local_bbbb", cliB, "Not here")
	desk.sync()

	lap := w.pc("Laptop", "code")
	lap.appOpen = true
	lap.sync()
	if n := lap.eng.WaitingForApp(); n != 2 {
		t.Fatalf("waiting = %d", n)
	}
	if err := lap.eng.Skip("session/local_bbbb", ""); err != nil {
		t.Fatal(err)
	}
	if n := lap.eng.WaitingForApp(); n != 1 {
		t.Fatalf("skip didn't take it off the list: %d", n)
	}
	if lap.eng.Skipped()["session/local_bbbb"] != "Not here" {
		t.Fatalf("skipped list: %v", lap.eng.Skipped())
	}
	lap.appOpen = false
	lap.sync()
	desk.appendLine(cliB, "newer")
	desk.sync()
	lap.sync()
	if s := lap.sessions(); len(s) != 1 || s[0] != "Wanted" {
		t.Fatalf("skipped item restored anyway: %v", s)
	}
	third := w.pc("Third", "code")
	third.sync()
	if s := third.sessions(); len(s) != 2 {
		t.Fatalf("skip on one PC affected another: %v", s)
	}
	if err := lap.eng.Unskip("session/local_bbbb"); err != nil {
		t.Fatal(err)
	}
	lap.sync()
	if s := lap.sessions(); len(s) != 2 {
		t.Fatalf("un-skip didn't restore it: %v", s)
	}
}

// Project folders live in the same server store; chat sync must leave them alone.
func TestProjectItemsIgnoredByChatSync(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Chat")
	if _, err := desk.eng.API.PutManifest(context.Background(), proto.KindProject, "prj_abc", proto.ManifestUpload{Blob: []byte("not a chat"), Refs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := desk.eng.Sync(context.Background()); err != nil {
		t.Fatalf("chat sync tripped over a project item: %v", err)
	}
	if p := desk.eng.Status().Pending; len(p) != 0 {
		t.Fatalf("project item queued as a chat restore: %+v", p)
	}
}

// Deleting for good removes the server's copy and nothing ever brings it back,
// not even from a PC that still has the chat and hadn't heard about it.
func TestDeleteFromServerForGood(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Keep")
	desk.addSession("local_bbbb", cliB, "Delete me")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.appOpen = true // the laptop has it waiting, hasn't loaded it
	lap.sync()

	if err := desk.eng.DeleteFromServer(context.Background(), proto.KindSession, "local_bbbb"); err != nil {
		t.Fatal(err)
	}
	it := serverItem(t, desk.eng, "local_bbbb")
	if it == nil || !it.Header.Purged || !it.Header.Removed {
		t.Fatalf("server list: %+v", it)
	}
	if _, blob, err := desk.eng.API.Get(context.Background(), proto.KindSession, "local_bbbb", 0); err != nil || len(blob) != 0 {
		t.Fatalf("the content is still on the server: %d bytes, %v", len(blob), err)
	}
	if err := lap.eng.BringBack(context.Background(), proto.KindSession, "local_bbbb"); err == nil {
		t.Fatal("brought back something deleted for good")
	}

	// The desktop still has the chat, keeps working on it, and it never returns to the server.
	desk.appendLine(cliB, "more work")
	desk.sync()
	if it := serverItem(t, desk.eng, "local_bbbb"); it == nil || !it.Header.Purged || it.Header.Version != 1 {
		t.Fatalf("uploaded again: %+v", it)
	}
	if s := desk.sessions(); len(s) != 2 {
		t.Fatalf("deleting from the server touched the desktop's own copy: %v", s)
	}
	// A PC that never heard about it doesn't get it either, and one that has it doesn't re-upload it.
	lap.appOpen = false
	lap.sync()
	if s := lap.sessions(); len(s) != 1 || s[0] != "Keep" {
		t.Fatalf("laptop restored a deleted chat: %v", s)
	}
	third := w.pc("Third", "code")
	third.addSession("local_bbbb", cliB, "Delete me")
	third.sync()
	if it := serverItem(t, desk.eng, "local_bbbb"); it == nil || it.Header.Version != 1 {
		t.Fatalf("a PC that hadn't heard of the deletion put it back: %+v", it)
	}
}

// The engine reports which folders chats use, and only says so when it really
// read the sessions: "no chats" is only believed from a good look.
func TestChatFolders(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	if _, ok := desk.eng.ChatFolders(); ok {
		t.Fatal("trusted before any sync ran")
	}
	desk.addSession("local_aaaa", cliA, "One")
	desk.addSession("local_bbbb", cliB, "Two")
	desk.sync()
	dirs, ok := desk.eng.ChatFolders()
	if !ok || len(dirs) == 0 {
		t.Fatalf("after a sync with chats: %v %v", dirs, ok)
	}
	for _, d := range dirs {
		if d != desk.project {
			t.Fatalf("unexpected folder %q (want %q)", d, desk.project)
		}
	}

	// every chat deleted in Claude: still a good look, and it's empty
	files, _ := filepath.Glob(filepath.Join(desk.sessionsDir(), "local_*.json"))
	for _, f := range files {
		os.Remove(f)
	}
	desk.sync()
	dirs, ok = desk.eng.ChatFolders()
	if !ok || len(dirs) != 0 {
		t.Fatalf("after deleting every chat: %v %v", dirs, ok)
	}

	// Claude's sessions folder not there at all: nothing can be concluded
	os.RemoveAll(filepath.Dir(filepath.Dir(desk.sessionsDir()))) // claude-code-sessions itself
	desk.sync()
	if _, ok := desk.eng.ChatFolders(); ok {
		t.Fatal("trusted an empty list when the sessions folder is missing")
	}
}
