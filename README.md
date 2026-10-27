# ClaudeSync

Pick up your Claude desktop-app sessions on any of your PCs. Each PC backs up
its Claude Code sessions to your own server on a schedule. Your other PCs
restore them automatically, with project folders remapped to wherever they
live on that machine.

- **Desktop client** (`claudesync`): GUI and tray icon; starts at login
- **Server** (`claudesync-server`): headless background service (Windows service or systemd)
- **Platforms:** Windows x64 and ARM64, Linux x64, ARM64 and ARMv7 (e.g. Raspberry Pi)

See [DESIGN.md](DESIGN.md) for how it works inside.

## What gets synced

| Synced | Not synced |
|---|---|
| Desktop app Code-tab sessions: title, model and settings, plus the full conversation | Your Claude login and any credentials |
| Working folders of sessions started without a project folder (default limit 200 MB each) | `settings.local.json` (meant to stay on one machine) |
| `~/.claude` settings, `CLAUDE.md`, commands, skills, agents, output styles | The desktop app's own MCP config (`claude_desktop_config.json`) |
| Per-project memory | |
| Project folders you choose: backed up, or kept in sync on every PC ([details](docs/PROJECT_SYNC.md)) | Project folders you leave Off |
| *(optional)* Terminal-only Claude Code sessions | |

## Server setup

Download the `claudesync-server` build for the machine that will host it.

**Linux** (as root):

```bash
sudo ./claudesync-server init       # choose the server password
sudo ./claudesync-server install    # runs it as a systemd service
```

**Windows** (in an Administrator terminal):

```bash
claudesync-server.exe init
claudesync-server.exe install
```

The server listens on two addresses:

- **`:8443`**: HTTPS for PCs on your network. The certificate is self-signed; each PC
  shows its fingerprint once, and you compare it against `claudesync-server fingerprint`.
  Allow this port through the firewall.
- **`127.0.0.1:8480`**: plain HTTP on the server machine only, for `cloudflared`.

### Cloudflare Tunnel

Point a tunnel hostname at the loopback port. Cloudflare provides HTTPS on
the internet side. Example `config.yml`:

```yaml
ingress:
  - hostname: sync.example.com
    service: http://localhost:8480
  - service: http_status:404
```

The server treats a request's `CF-Connecting-IP` header as the caller's
address only when the request arrives through that loopback port. Failed
password attempts are rate-limited per address.

### Other server commands

| Command | Use |
|---|---|
| `claudesync-server reset-password` | Forgot the password: sets a new one from the server machine. Add `--logout-all` to also sign every PC out. |
| `claudesync-server devices` | Lists connected PCs |
| `claudesync-server status` / `stop` / `start` / `uninstall` | Controls the service |

Server data lives in `/var/lib/claudesync` (Linux, as root) or
`%ProgramData%\ClaudeSync` (Windows). Override it with `--data DIR`.

## Client setup

1. Run `claudesync` on each PC.
2. Enter a name for the PC, the server's network address (`192.168.x.x:8443`),
   your tunnel address, and the server password.
3. Confirm the fingerprint.
4. On the **first** PC, choose a **sync passphrase**. On every other PC, enter the
   same passphrase.

The passphrase encrypts your chats before they leave the PC (Argon2id plus
XChaCha20-Poly1305), so the server only ever stores ciphertext. **It can't be
reset.** If it's lost, existing backups can't be decrypted.

After setup ClaudeSync lives in the tray:

- **Backs up** every 30 minutes and **syncs** every 30 minutes. The two intervals
  are set separately in Settings.
- **Every sync backs up this PC first**, then loads what's new from your other PCs.
  It also syncs right after start-up, so a PC you come back to catches up.
- **Nothing is overwritten without a copy.** Before ClaudeSync replaces any file,
  it saves the current version in a local snapshot. The **Recover** tab puts any
  snapshot back. ClaudeSync never deletes anything, here or on the server.
- **Chats that vanish come back.** If a session that was backed up disappears from a
  PC without being deleted in Claude, the next sync restores it. Sessions you delete
  in Claude stay deleted on that PC.
- **If Claude is open**, updates wait. They load the moment Claude is closed, however
  you close it. ClaudeSync can also ask Claude to quit, but it never force-closes it.
- **Recover lost chats.** If a conversation is still on disk but missing from Claude's
  sidebar, the **Recover** tab lists it and can add it back. Earlier parts of chats you
  still have are marked, so you don't get duplicates.
- **If the same session changed on two PCs**, it keeps both. The other PC's version
  appears as a separate session named "… (from Laptop)".
- **Projects** are matched by git remote, then by folder name. Anything it can't find
  waits in the **Projects** tab until you pick the folder.
- **History:** the server keeps the last 20 versions of everything (you can change
  this in the **Server** tab). Any version can be restored from the **History** tab.
- **Remove from the backup:** in the **History** tab, **Remove from server backup** stops
  an item syncing to every PC without deleting it from any PC's Claude. It can be
  brought back for 30 days ("Show removed"), then the server deletes it.
- **Skip on one PC:** **Skip** next to an item waiting to restore keeps it off that PC
  only. **Skipped on this PC…** lists skipped items so you can un-skip them.
- **Password changes:** the **Server** tab can change the server password. Connected
  PCs stay connected.

Run `claudesync --scan` in a terminal to list exactly what would be synced on
that PC without uploading anything.

## Limits and caveats

- **Same Claude account on every PC.** Sessions are stored under the account's
  folder in the desktop app's data.
- **The desktop app's session format is undocumented.** If an app update changes
  it, ClaudeSync keeps backing up but pauses restoring desktop sessions and tells
  you, until ClaudeSync is updated.
- **Deleting a session doesn't spread to other PCs.** It also won't be brought back
  on the PC where you deleted it, unless it changes again elsewhere.
- **Worktree sessions** resume in the main project folder on other PCs.
- **Linux** has no official desktop app. ClaudeSync looks for community builds in
  `~/.config/Claude`, `~/.config/claude-desktop`, Flatpak and Snap folders. If it finds
  more than one with sessions, it won't load updates into either until you pick the
  right one in Settings.

## Upgrading from 0.1

0.1 had two serious bugs: sessions restored from Linux onto Windows got paths Claude
couldn't read, and an unreadable record could replace the good copy on the server.
0.2 repairs files already affected (saving the originals in a snapshot first),
never uploads unreadable records, and restores the newest *readable* version from
the server's history.

## Building

Builds for every platform come from GitHub Actions (`.github/workflows/build.yml`).
Push a `v*` tag to publish a release. To build locally you need Go and, for the
GUI client, a C compiler:

```bash
go test ./...
go build ./cmd/claudesync-server
go build -ldflags "-H=windowsgui" ./cmd/claudesync
```
