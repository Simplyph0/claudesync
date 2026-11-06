package syncer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// How two copies of the same chat relate, judged by the messages in them.
type convo int

const (
	convoSame        convo = iota // the same messages (only settings like "last opened" differ)
	convoRemoteAhead              // the other PC's copy has everything this PC has, and more
	convoLocalAhead               // this PC's copy has everything the other has, and more
	convoDiverged                 // each has messages the other lacks
)

// messageIDs collects an identity for every message line in a chat's
// transcripts. Claude gives each message a uuid; lines without one (rare)
// are identified by their content. File paths in the text don't matter, so
// the same chat on a Windows and a Linux PC compares equal.
func messageIDs(it *Item) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, f := range it.Files {
		if f.Role != RoleTranscript || !strings.HasSuffix(f.Rel, ".jsonl") {
			continue
		}
		r := bufio.NewReaderSize(bytes.NewReader(f.Data), 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if line = bytes.TrimSpace(line); len(line) > 0 {
				ids[lineID(line)] = struct{}{}
			}
			if err != nil {
				break
			}
		}
	}
	return ids
}

func lineID(line []byte) string {
	var k struct {
		UUID      string `json:"uuid"`
		MessageID string `json:"messageId"`
		LeafUUID  string `json:"leafUuid"`
	}
	if json.Unmarshal(line, &k) == nil {
		switch {
		case k.UUID != "":
			return "u:" + k.UUID
		case k.MessageID != "":
			return "m:" + k.MessageID
		case k.LeafUUID != "":
			return "l:" + k.LeafUUID
		}
	}
	sum := sha256.Sum256(line)
	return "h:" + hex.EncodeToString(sum[:])
}

// compareConvos says which copy of a chat is further along. The record file
// Claude keeps for each chat changes whenever you merely open it, so two
// copies that differ only there are not a conflict.
func compareConvos(local, remote *Item) convo {
	l, r := messageIDs(local), messageIDs(remote)
	localHasExtra, remoteHasExtra := false, false
	for id := range l {
		if _, ok := r[id]; !ok {
			localHasExtra = true
			break
		}
	}
	for id := range r {
		if _, ok := l[id]; !ok {
			remoteHasExtra = true
			break
		}
	}
	switch {
	case localHasExtra && remoteHasExtra:
		return convoDiverged
	case remoteHasExtra:
		return convoRemoteAhead
	case localHasExtra:
		return convoLocalAhead
	}
	return convoSame
}
