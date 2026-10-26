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
| Restore while the desktop app runs | Notify "updates ready, restart Claude?"; on yes close Claude, restore, reopen |
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

- **Backup** every `backupInterval` (default 30 min): scan, build portable
  items, upload changed ones (by content hash).
- **Restore** every `restoreInterval` (default 30 min, separate): fetch item
  headers since the last seen sequence; for each item last written by another
  PC, apply it unless the local copy also changed since the last sync →
  conflict → restore as a copy.
- Desktop session restores are queued while the Claude app is running; the
  client notifies and offers restart.

## Server storage

`<data>/server.json` (password hash = Argon2id, settings),
`<data>/devices.json`, `<data>/items/<kind>/<id>/<version>.blob` +
`meta.json`; old versions beyond the retention count are deleted.
Plain HTTPS on the LAN port with a self-signed cert; Cloudflare Tunnel points
at the same port (with `noTLSVerify`) or a separate plain-HTTP localhost port.
