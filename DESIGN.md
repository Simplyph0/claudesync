# ClaudeSync — design

Keeps Claude desktop-app (Code tab) sessions, Claude Code conversations, and
Claude settings/skills in sync between one person's PCs through a self-hosted
server.

## Decisions (from the owner)

| Topic | Decision |
|---|---|
| Data synced | Desktop-app session records, Claude Code transcripts, settings & skills, folderless-session working folders |
| Never synced | Login tokens / credentials (`.credentials.json`, app sign-in state, caches) |
| Stack | Go; client GUI in Fyne; server is pure Go |
| Platforms | Windows x64 + ARM64; Linux x64 + ARM64 + ARMv7. No macOS |
| Users | One person per server, one shared server password |
| Network | Client tries the LAN address first (self-signed cert, pinned on first login), then the Cloudflare Tunnel hostname |
| Security | TLS in transit + end-to-end encryption: data is encrypted on the PC with a key derived from a sync passphrase; the server stores only ciphertext |
| Password reset | Any logged-in client can change the server password; connected PCs keep working via device tokens; `claudesync-server reset-password` on the server machine for a forgotten password |
| Server form | Headless background service (Windows service / systemd unit) |
| History | Server keeps the last N versions per item, default 20, configurable |
| Conflicts | Keep both: the other PC's diverged copy is restored as a separate session titled "(from <PC>)" |
| Restore while the desktop app runs | Wait; apply as soon as Claude is closed. "Close Claude and load updates" asks Claude to quit normally and reopens it; never force-closes |
| Unknown desktop session format | Keep backing up raw files, pause restoring desktop sessions, notify |
| Client UX | Tray icon, start at login, desktop notifications; backup interval default 30 min, restore interval set separately |
| Path mapping | Auto-match projects by git remote URL, then folder name; GUI lets you override per PC |

## Data model

The client turns local files into **items**. An item is the unit that is
versioned, encrypted and uploaded.

| Item kind | Key | Contents |
|---|---|---|
| `session` | desktop `sessionId` (or CLI session id for CLI-only sessions) | desktop session JSON (if any) + transcript `.jsonl` + transcript sidecar folder + project info + scratch folder (if folderless) |
| `config` | relative path under `~/.claude` | one settings/skills/commands/agents/CLAUDE.md file |

Every item is stored in **portable form**: absolute paths are replaced with
placeholders before upload and expanded on restore:

- `{{PROJECT}}` — the session's project root
- `{{HOME}}` — the user's home folder
- `{{SCRATCH}}` — the session's app-made scratch folder

Both `\`-escaped (`C:\\Users\\...`) and `/` forms are rewritten, plus the
`~/.claude/projects/<encoded-path>` folder name (every non-alphanumeric char
becomes `-`).

A project is described by `{name, gitRemote}`; each client resolves that to a
local folder (git remote match → folder-name match under configured search
roots → manual mapping in the GUI). Sessions whose project can't be resolved
wait in an "Unmapped" list in the GUI.

## Protocol (HTTPS, JSON + binary blobs)

- `POST /api/login {password, deviceName}` → `{deviceId, token}`
- `POST /api/password {old, new}` (token) — change server password
- `GET  /api/items?since=<seq>` (token) → item headers changed since a sequence number
- `PUT  /api/items/<kind>/<id>` (token, body = encrypted blob, headers carry
  base version + content hash) → new version number; `409` if base version is stale
- `GET  /api/items/<kind>/<id>[?v=N]` (token) → encrypted blob
- `GET  /api/items/<kind>/<id>/versions` (token) → version list
- `GET  /api/devices`, `DELETE /api/devices/<id>` (token) — list / revoke PCs
- `GET  /api/vault` / `PUT /api/vault` — salt + encrypted check value used to
  verify the sync passphrase (server can't read it)

Blobs over 64 MB are uploaded in chunks (Cloudflare's request body limit is
100 MB on the free plan).

## Encryption

- Key = Argon2id(sync passphrase, salt from server vault).
- Each item blob = XChaCha20-Poly1305(zstd(tar of item files)), with the
  item kind/id as associated data.
- Item ids are sent in clear (random UUIDs, no content); titles are inside the blob.

## Sync loop (client)

Safety rules, in priority order:

1. **Never lose data.** ClaudeSync never deletes a file, locally or on the server.
   Before it replaces a local file, it saves the current version in a local snapshot
   (`snapshots/*.tar.zst`, the last 40 are kept). Before the first change in a sync,
   it also saves the whole session list. A file whose copy can't be saved is not
   replaced.
2. **Back up first.** Every sync uploads this PC's changes before restoring anything.
3. **Never touch a running Claude.** Session restores wait until the desktop app is
   closed. ClaudeSync only ever asks Claude to quit; it never kills it. It relaunches
   Claude only through Claude's own launcher.

Each sync:

- **Repair:** while Claude is closed, fix JSON files broken by 0.1 (unescaped
  backslashes in paths restored from Linux), saving the originals first.
- **Backup:** upload items whose content changed. Unreadable desktop records are never
  uploaded. Records in an unknown but valid format are uploaded under a separate
  `raw_` id, so they never replace a readable session.
- **Reconcile:** compare the full server list against this PC.
  - **Behind:** the server version is newer than the one this PC last synced, so restore it.
  - **Missing:** this PC had the item, it is gone, and Claude left no `deleted_` marker,
    so restore it (self-heal).
  - **Deleted in Claude:** there is a `deleted_` marker, so don't restore it here.
- **Conflict:** the item changed both here and elsewhere. For sessions, keep both: the
  other PC's version becomes a copy titled "(from <PC>)". For settings and memory, the
  newer version wins; this PC's copy is kept in the snapshot.
- **Unreadable server version:** if the newest server version is unreadable (uploaded by
  0.1), use the newest readable one from history.

The Recover tab lists conversations on disk that no sidebar entry points to
(orphans). It marks the ones that are earlier parts of chats still listed, and can
add the rest back by cloning the newest local record's format. It only ever creates
new files.
## Server storage

`<data>/server.json` (password hash = Argon2id, settings),
`<data>/devices.json`, `<data>/items/<kind>/<id>/<version>.blob` +
`meta.json`; old versions beyond the retention count are deleted.
Plain HTTPS on the LAN port with a self-signed cert; Cloudflare Tunnel points
at the same port (with `noTLSVerify`) or a separate plain-HTTP localhost port.
