package syncer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/google/uuid"
)

// An Orphan is a Claude Code conversation on disk that no desktop-app
// session points at, so it doesn't show in Claude's sidebar. That's what is
// left behind when a session's sidebar entry is lost.
type Orphan struct {
	CliID    string
	Path     string // the .jsonl transcript
	Cwd      string
	Title    string
	Started  time.Time
	Modified time.Time
	Size     int64
	// PartOf names a session already in the sidebar that this conversation
	// is an earlier part of (Claude starts a new file when a session is
	// moved or resumed). Adding it back would only duplicate that session.
	PartOf string
}

var ErrNoTemplate = errors.New("there are no Claude sessions on this PC to copy the session format from; start one new chat in Claude, close Claude, and try again")

// Orphans lists conversations that aren't in Claude's sidebar, newest first.
func (s *Scanner) Orphans() ([]Orphan, error) {
	if s.Loc.SessionsDir() == "" {
		return nil, errors.New("the Claude desktop app's data folder wasn't found on this PC")
	}
	recs, _ := s.desktopRecords()
	used := map[string]bool{}
	titles := map[string]string{}
	for _, r := range recs {
		used[r.cliID] = true
		if r.title != "" {
			titles[normTitle(r.title)] = r.title
		}
	}
	var out []Orphan
	for cli, dir := range s.transcriptIndex() {
		if used[cli] {
			continue
		}
		p := filepath.Join(dir, cli+".jsonl")
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			continue
		}
		o := readOrphan(p)
		if o.Cwd == "" {
			continue // not a real conversation (no working folder recorded)
		}
		o.CliID, o.Path, o.Modified, o.Size = cli, p, info.ModTime(), info.Size()
		if o.Started.IsZero() {
			o.Started = info.ModTime()
		}
		if o.Title == "" {
			o.Title = "Recovered chat " + o.Started.Format("2006-01-02 15:04")
		}
		o.PartOf = titles[normTitle(o.Title)]
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// readOrphan pulls the working folder, a title and the start time out of a transcript.
func readOrphan(path string) Orphan {
	var o Orphan
	f, err := os.Open(path)
	if err != nil {
		return o
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	firstUser := ""
	for n := 0; sc.Scan() && n < 5000; n++ {
		var v struct {
			Type        string          `json:"type"`
			Cwd         string          `json:"cwd"`
			CustomTitle string          `json:"customTitle"`
			Summary     string          `json:"summary"`
			Timestamp   string          `json:"timestamp"`
			IsMeta      bool            `json:"isMeta"`
			Message     json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		if o.Cwd == "" && v.Cwd != "" {
			o.Cwd = v.Cwd
		}
		if v.CustomTitle != "" {
			o.Title = v.CustomTitle
		} else if v.Type == "summary" && v.Summary != "" && o.Title == "" {
			o.Title = v.Summary
		}
		if o.Started.IsZero() && v.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, v.Timestamp); err == nil {
				o.Started = t
			}
		}
		if firstUser == "" && v.Type == "user" && !v.IsMeta {
			firstUser = userText(v.Message)
		}
	}
	if o.Title == "" && firstUser != "" {
		o.Title = firstUser
	}
	o.Title = strings.Join(strings.Fields(o.Title), " ")
	if r := []rune(o.Title); len(r) > 70 {
		o.Title = string(r[:67]) + "…"
	}
	return o
}

func normTitle(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Join(strings.Fields(s), " "), "…"))
}

func userText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if strings.HasPrefix(strings.TrimSpace(s), "<") { // command/system wrappers, not what the user typed
			return ""
		}
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" && !strings.HasPrefix(strings.TrimSpace(p.Text), "<") {
				return p.Text
			}
		}
	}
	return ""
}

// Fields copied from the template that belong to that one session and must
// not carry over to a recovered one.
var perSessionFields = []string{
	"postTurnSummary", "postTurnSummaryFor", "lastAssistantUuid", "gitAnchors", "gitAnchorsLookupOnly",
	"gitAnchorsFolderRealpath", "latestUserFrameAt", "spawnSeed", "scratchPromptRecents", "scratchOfferFolder",
	"sessionPermissionUpdates", "alwaysAllowedReasons", "steeredByRemoteClient", "lastSpawnRootDetected",
}

// template returns the newest desktop session record on this PC, used as the
// pattern for recovered sessions so they match this app version's format.
func (s *Scanner) template() (*desktopRecord, error) {
	recs, _ := s.desktopRecords()
	var best *desktopRecord
	var bestTime time.Time
	for _, r := range recs {
		if !r.known {
			continue
		}
		if info, err := os.Stat(r.path); err == nil && info.ModTime().After(bestTime) {
			best, bestTime = r, info.ModTime()
		}
	}
	if best == nil {
		return nil, ErrNoTemplate
	}
	return best, nil
}

// Adopt adds orphaned conversations to Claude's sidebar as new sessions.
// It only creates new files; it never changes or removes existing ones.
// Claude must be closed.
func (s *Scanner) Adopt(orphans []Orphan) (int, error) {
	if s.appRunning() {
		return 0, errors.New("close the Claude app first")
	}
	tpl, err := s.template()
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(tpl.path)
	n := 0
	var errs []error
	for _, o := range orphans {
		dec := json.NewDecoder(bytes.NewReader(tpl.raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return n, fmt.Errorf("reading the session format: %w", err)
		}
		for _, k := range perSessionFields {
			delete(m, k)
		}
		id := "local_" + uuid.NewString()
		ms := func(t time.Time) json.Number { return json.Number(fmt.Sprint(t.UnixMilli())) }
		m["sessionId"] = id
		m["cliSessionId"] = o.CliID
		m["cwd"] = o.Cwd
		m["originCwd"] = o.Cwd
		m["title"] = o.Title
		m["titleSource"] = "user"
		m["createdAt"] = ms(o.Started)
		m["lastActivityAt"] = ms(o.Modified)
		m["lastFocusedAt"] = ms(o.Modified)
		m["isArchived"] = false
		out, err := json.Marshal(m)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dst := filepath.Join(dir, id+".json")
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, werr := f.Write(out)
		if err := errors.Join(werr, f.Close()); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// TranscriptPath is where Claude Code keeps a session's conversation.
func (s *Scanner) TranscriptPath(cwd, cliID string) string {
	return filepath.Join(s.Loc.ProjectsDir(), claude.EncodeProjectDir(cwd), cliID+".jsonl")
}
