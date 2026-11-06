package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendMsg adds a message with a uuid, as Claude writes them.
func (p *pc) appendMsg(cli, uuid, text string) {
	f, err := os.OpenFile(filepath.Join(p.transcriptDir(), cli+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	l, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "sessionId": cli, "message": text})
	f.Write(append(l, '\n'))
	f.Close()
}

// openChat does what Claude does when you merely look at a chat: it rewrites
// the chat's record (last-opened time and the like) without adding a message.
func (p *pc) openChat(id string, at int64) {
	path := filepath.Join(p.sessionsDir(), id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		p.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		p.t.Fatal(err)
	}
	m["lastFocusedAt"], m["lastActivityAt"] = at, at
	writeJSON(p.t, path, m)
}

func (p *pc) transcript(cli string) string {
	b, _ := os.ReadFile(filepath.Join(p.transcriptDir(), cli+".jsonl"))
	return string(b)
}

// The other PC continued the chat while you only opened it here: you get the
// new messages, and no second copy of the chat appears.
func TestOpeningAChatIsNotAConflict(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Fix the thing")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()

	desk.appendMsg(cliA, "uuid-2", "more from desktop")
	desk.sync()
	lap.openChat("local_aaaa", 1791500000000)
	lap.sync()
	if s := lap.sessions(); len(s) != 1 {
		t.Fatalf("a duplicate appeared on the laptop: %v", s)
	}
	if !strings.Contains(lap.transcript(cliA), "more from desktop") {
		t.Fatal("the laptop didn't get the desktop's messages")
	}
	desk.sync()
	lap.sync()
	for i, p := range []*pc{desk, lap} {
		if s := p.sessions(); len(s) != 1 {
			t.Fatalf("PC %d: %v", i, s)
		}
	}
}

// Both PCs only opened the chat: nothing to keep twice.
func TestBothOpenedTheSameChat(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Fix the thing")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()

	desk.openChat("local_aaaa", 1791500000000)
	lap.openChat("local_aaaa", 1791500005000)
	desk.sync()
	lap.sync()
	desk.sync()
	lap.sync()
	for i, p := range []*pc{desk, lap} {
		if s := p.sessions(); len(s) != 1 {
			t.Fatalf("PC %d got a duplicate: %v", i, s)
		}
	}
}

// You continued the chat here while the other PC only opened it: your messages
// are kept and travel to the other PC, with no duplicate anywhere.
func TestOnlyThisPCContinuedTheChat(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Fix the thing")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()

	lap.appendMsg(cliA, "uuid-2", "more from laptop")
	desk.openChat("local_aaaa", 1791500000000)
	desk.sync()
	lap.sync() // its upload is turned down (the desktop got there first), then it keeps its own version
	lap.sync()
	desk.sync()
	for i, p := range []*pc{desk, lap} {
		if s := p.sessions(); len(s) != 1 {
			t.Fatalf("PC %d got a duplicate: %v", i, s)
		}
		if !strings.Contains(p.transcript(cliA), "more from laptop") {
			t.Fatalf("PC %d is missing the laptop's messages", i)
		}
	}
}

// Messages added on both PCs since the last sync are still kept as two chats.
func TestRealConflictStillKeepsBoth(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Fix the thing")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()

	desk.appendMsg(cliA, "uuid-d", "from desktop")
	lap.appendMsg(cliA, "uuid-l", "from laptop")
	lap.openChat("local_aaaa", 1791500000000)
	desk.sync()
	lap.sync()
	s := lap.sessions()
	if len(s) != 2 || !has(s, "Fix the thing (from Desktop)") {
		t.Fatalf("a real conflict lost a version: %v", s)
	}
	if !strings.Contains(lap.transcript(cliA), "from laptop") {
		t.Fatal("the laptop's own version was overwritten")
	}
}

func TestCompareConvos(t *testing.T) {
	mk := func(lines ...string) *Item {
		return &Item{Files: []File{{Role: RoleTranscript, Rel: "c.jsonl", Data: []byte(strings.Join(lines, "\n") + "\n")}}}
	}
	a := `{"uuid":"1","message":"x"}`
	b := `{"uuid":"2","message":"y"}`
	c := `{"uuid":"3","message":"z"}`
	for name, tc := range map[string]struct {
		l, r *Item
		want convo
	}{
		"same":           {mk(a, b), mk(a, b), convoSame},
		"remote ahead":   {mk(a), mk(a, b), convoRemoteAhead},
		"local ahead":    {mk(a, b), mk(a), convoLocalAhead},
		"diverged":       {mk(a, b), mk(a, c), convoDiverged},
		"paths in text":  {mk(`{"uuid":"1","m":"C:\\x"}`), mk(`{"uuid":"1","m":"/home/x"}`), convoSame},
		"no id lines":    {mk(`{"type":"summary"}`), mk(`{"type":"summary"}`, `{"type":"other"}`), convoRemoteAhead},
		"nothing at all": {&Item{}, &Item{}, convoSame},
		"subagent file":  {mk(a), &Item{Files: []File{{Role: RoleTranscript, Rel: "c.jsonl", Data: []byte(a + "\n")}, {Role: RoleTranscript, Rel: "c/sub.jsonl", Data: []byte(b + "\n")}}}, convoRemoteAhead},
	} {
		if got := compareConvos(tc.l, tc.r); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}
