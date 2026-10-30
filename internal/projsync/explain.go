package projsync

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"strings"

	"github.com/Simplyph0/claudesync/internal/client"
)

// Problem is an error in plain words: what went wrong and what to do.
type Problem struct {
	What, Fix string
}

// Explain turns a sync error into something a person can act on. folder is
// the project's folder, used when the error is about it.
func Explain(err error, folder string) Problem {
	if err == nil {
		return Problem{}
	}
	raw := err.Error()
	low := strings.ToLower(raw)
	var api *client.APIError
	var netErr net.Error
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	switch {
	case errors.Is(err, client.ErrServerTooOld):
		return Problem{"Your ClaudeSync server is too old to delete things for good. It only removed the item, which can still be brought back.",
			"Update claudesync-server on your server to this version, then delete it again."}
	case errors.Is(err, client.ErrPurged):
		return Problem{"This folder's copy on the server was deleted for good.",
			"Open the folder's card: it asks whether to start a new copy from this folder."}
	case errors.Is(err, client.ErrRemoved):
		return Problem{"This folder's copy on the server was removed, so it can't be backed up or synced any more.",
			"Bring it back in the History tab (\"Show removed\")."}
	case errors.Is(err, client.ErrUnauthorized):
		return Problem{"The server doesn't recognise this PC any more.",
			"Open the Server tab and sign in again. Your files and settings are untouched."}
	case errors.Is(err, client.ErrPinMismatch):
		return Problem{"The server's certificate changed since you confirmed it.",
			"If you reinstalled the server, confirm its new fingerprint in the Server tab. If you didn't, don't continue: someone may be intercepting the connection."}
	case errors.Is(err, fs.ErrNotExist) && folder != "" && has(strings.ToLower(folder)):
		return Problem{"The folder " + folder + " isn't there.",
			"If it's on a drive or share, reconnect it. If you moved or renamed it, open Details and use Change folder. Nothing was changed or deleted."}
	case errors.Is(err, fs.ErrPermission), has("permission denied", "access is denied"):
		return Problem{"ClaudeSync isn't allowed to read or change a file in this folder.",
			"Check the folder's permissions, or add the file to \"Don't sync these\" in Details. Everything else keeps syncing. Details below name the file."}
	case has("no space left", "not enough space", "disk full"):
		return Problem{"This PC's disk is full.", "Free some space, then press Sync now. Nothing was lost."}
	case has("being used by another process", "text file busy", "sharing violation"):
		return Problem{"A file is open in another program, so it can't be replaced.",
			"Close the program that has it open. ClaudeSync tries again on the next sync."}
	case has("message authentication failed"):
		return Problem{"This PC couldn't decrypt what's on the server. It's probably using a different sync passphrase than your other PCs.",
			"Open Settings and check the passphrase; every PC must use the same one. It can't be reset."}
	case has("changed while uploading", "changed while being read"):
		return Problem{"A file kept changing while it was being copied.", "Save and close it. ClaudeSync retries on the next sync."}
	case has("doesn't match its checksum"):
		return Problem{"A file arrived damaged from the server.", "Press Sync now to retry. If it keeps happening, check the server's disk."}
	case has("is not a folder"):
		return Problem{folder + " is a file, not a folder.", "Open Details and choose the folder again."}
	case errors.As(err, &api) && api.Status == 413:
		return Problem{"The server refused something as too big.",
			"If you use a Cloudflare Tunnel its upload limit may be too low. Skip the big file in Details, or sync over your home network."}
	case errors.As(err, &api) && api.Status >= 500:
		return Problem{"Your ClaudeSync server hit an error.", "Check its log on the server machine. ClaudeSync retries on the next sync."}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr),
		has("connection refused", "no route to host", "no such host", "network is unreachable", "i/o timeout", "unexpected eof", "connection reset"):
		return Problem{"Can't reach your ClaudeSync server.",
			"Check that the server is running and this PC is online. ClaudeSync retries on the next sync; nothing is lost."}
	case errors.Is(err, context.Canceled):
		return Problem{"The sync was stopped.", "Press Sync now to run it again."}
	}
	short := raw
	if len(short) > 200 {
		short = short[:200] + "…"
	}
	return Problem{"Sync failed: " + short, "Press Sync now to try again. If it keeps failing, send the details below to whoever maintains ClaudeSync."}
}
