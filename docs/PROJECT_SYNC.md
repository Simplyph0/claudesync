# Project folder sync: blueprint

Status: built in v0.4.0. Everything below is in the app; the settings are under Settings → Project folders.

## The short version

ClaudeSync syncs your chats today. This adds your project folders too.

- You choose which folders to sync: **Off**, **Back up only**, or **Sync**.
- **Back up only** keeps a versioned, encrypted copy on your server. Nothing on other
  PCs changes unless you ask to download a copy.
- **Sync** keeps the folder the same on every PC you link it on.
- **When the same file changed on two PCs**, you pick what happens, per project:
  - **Keep both** (default): both versions end up on every PC. The PC that spots the
    clash keeps its own file in place, and the other version sits next to it as
    `main (from Laptop).cpp`.
  - **Newest wins:** the most recently edited copy replaces the other. The replaced
    copy goes to ClaudeSync's trash, never just disappears.
  - **Ask me:** ClaudeSync lists the clash and you choose for each file.
- **Text files where the two PCs changed different parts** are merged automatically,
  line by line, the way git does. Most clashes never reach you.
- **Folders merge as a union.** A file that exists on only one PC is copied to the other.
- **Nothing is ever lost.** Any file that gets replaced or removed first goes to a local
  trash, kept for 30 days and restorable from the Recover tab. The server also keeps
  the last 20 versions of each project.

### About "newest replaces older"

The instinct is right, but on its own it has two traps, and the design avoids both.

1. **Most files didn't really change on both PCs.** If you edited `a.cpp` on the
   desktop and never touched it on the laptop, the desktop's version should just win,
   whatever the clocks say. ClaudeSync knows this because it remembers what each file
   looked like the last time the PCs agreed (the *base*). Only files changed on
   **both** sides since then are real clashes, and those are rare.
2. **"Newest" is decided by file timestamps, which lie.** PC clocks drift, and
   copying, unzipping or `git checkout` reset timestamps. Newest-wins is offered as
   a choice, but the losing copy always goes to the trash, never vanishes.

## What it's for

| Project kind | Recommended setting | Why |
|---|---|---|
| Not in git (e.g. Arduino sketches, notes) | **Sync** | Nothing else keeps these in step across PCs |
| In git and pushed to GitHub (e.g. PixelBit) | **Back up only**, or Off | Git already syncs it; two-way file sync on top of git can fight with branches |
| In git, but you never push | **Sync** with the git guard (below) | Gets you your uncommitted work on the other PC |

## How it fits the current design

```
            this PC                                server (sees only ciphertext)
 ┌──────────────────────────┐              ┌──────────────────────────────────┐
 │ project folder           │   chunks     │ chunk store (dedup, refcounted)   │
 │  → scan (ignore rules)   │ ───────────► │  c/ab/ab12… (encrypted 1–4 MB)    │
 │  → split into chunks     │              │                                  │
 │  → encrypt               │  manifest    │ items/project/<id>/<v>.blob      │
 │  → manifest (file list)  │ ───────────► │  (versioned like sessions, 20 kept)│
 └──────────────────────────┘              └──────────────────────────────────┘
```

- A project's **manifest** is a new item kind, `project`, in the existing versioned
  item store. It's encrypted, keeps 20 versions, can be removed and brought back, and
  shows in History, all for free.
- File **contents** go in a new **chunk store**. A large file is split into pieces
  and only changed pieces upload. Identical pieces (the same library in two projects,
  or a file that didn't change between versions) are stored once.
- Projects are identified the way they are today (git remote, then folder name), and
  each PC maps them to its own folder in the Projects tab.

### Manifest (encrypted, one per project version)

```json
{
  "format": 1,
  "project": {"key": "name:esp32_oled_clock", "name": "esp32_oled_clock"},
  "device": "James-PC",
  "git": {"branch": "main", "head": "a1b2c3d", "dirty": true},
  "files": [
    {"path": "esp32_oled_clock.ino", "size": 18234, "mtime": "2026-10-26T21:40:12Z",
     "mode": "0644", "hash": "sha256:…", "chunks": ["k:9f2c…", "k:77ab…"]},
    {"path": "lib/display.h", "size": 912, "mtime": "…", "hash": "…", "chunks": ["k:…"]}
  ],
  "deleted": [{"path": "old_test.ino", "at": "2026-10-25T10:00:00Z"}]
}
```

- **Paths** are relative to the project folder and always use `/`, so a project moves
  cleanly between Windows (`C:\Users\James\Documents\Arduino\x`) and Linux (`~/Arduino/x`).
- **`deleted`** records files removed since the last version, so a deletion on one PC
  isn't undone by a stale copy on another.
- **`git`** records the branch and commit, for the git guard.

### Chunks

- **Splitting:** files are split with content-defined chunking (FastCDC; about 1 MB
  on average, 4 MB at most), so an insert near the start of a file doesn't change
  every later chunk. Files under 64 KB are stored as one chunk.
- **Chunk IDs:** `k:` + HMAC-SHA256(chunk-ID key, plaintext), where the chunk-ID key
  is derived from your sync passphrase. The server can tell that two chunks are the
  same (which is what makes dedup work), but can't tell what's in them or guess
  common files.
- **Encryption:** each chunk is encrypted with XChaCha20-Poly1305, the same as
  everything else. The chunk ID is bound in as associated data, so a chunk can't be
  swapped for another.
- **Size limit:** chunks are at most 4 MB, well under Cloudflare's 100 MB request limit.

### Server additions

| Endpoint | Purpose |
|---|---|
| `POST /api/chunks/missing` (list of IDs) | Which of these chunks the server doesn't have yet |
| `PUT /api/chunks/{id}` | Upload one encrypted chunk |
| `GET /api/chunks/{id}` | Download one |
| Manifest upload adds header `X-Chunk-Refs` | The chunk IDs this version uses (opaque IDs, not contents) |

- **Garbage collection:** the server counts how many versions use each chunk. When a
  version is pruned (beyond the last 20) or a removed project is purged after 30 days,
  chunks no one uses any more are deleted.
- **Disk use:** the Server tab shows space used per project.

## What gets synced in a project folder

Always skipped:

- `.git/` (syncing git's internals between PCs can corrupt the repository)
- Dependency and build folders: `node_modules`, `.venv`, `venv`, `__pycache__`,
  `build`, `dist`, `target`, `.pio`, `.gradle`, `.next`, `bin/Debug`, `obj`
- OS and editor junk: `.DS_Store`, `Thumbs.db`, `desktop.ini`, `*.swp`, `~$*`
- ClaudeSync's own conflict copies, until you resolve them

Also skipped:

- For git projects, anything `.gitignore` excludes.
- Anything listed in an optional **`.claudesyncignore`** in the project (gitignore syntax).

Limits, with a warning in the Projects tab when hit:

- Single file: 200 MB (configurable). Larger files are skipped.
- Whole project: 2 GB (configurable). ClaudeSync asks before backing up something
  larger.

Special cases:

| Case | Handling |
|---|---|
| Symlinks | Stored as links, not followed. Recreated on Linux; on Windows, skipped with a note |
| Names Windows can't use (`a:b.txt`, `CON`, trailing dots) | Synced between Linux PCs; skipped on Windows with a note in the Projects tab |
| Two names differing only in case (`Readme.md` / `README.md`) | On Windows or macOS one would overwrite the other, so the second arrives as a conflict copy |
| Executable bit (Linux) | Kept (`mode` in the manifest) |
| Line endings | Never converted. Files are copied byte for byte |
| A file still being written | Skipped until it hasn't changed for 10 s. ClaudeSync also checks size and time before and after reading, and retries if either moved |

## The sync algorithm

Each PC remembers, per project, the manifest version it last agreed with the server:
the **base**. Every sync compares three lists (this PC, the server's latest, and the
base) file by file:

| This PC vs base | Server vs base | Result |
|---|---|---|
| same | same | nothing |
| changed | same | upload this PC's version |
| same | changed | download the server's version (this PC's copy goes to trash first) |
| changed | changed, to the same content | nothing (both made the same edit) |
| changed | changed differently | **clash** (see below) |
| new file | absent | upload |
| absent | new file | download |
| new | new, different content | **clash** |
| deleted | same | record the deletion; other PCs move their copy to trash |
| same | deleted | move this PC's copy to trash |
| deleted | changed | **the change wins**: file restored with the server's version |
| changed | deleted | **the change wins**: this PC's version uploads again |

**Folders** aren't tracked on their own. A folder exists if it has files in it, so
"merging folders" is simply the union of the files above. Empty folders are recorded
so they're recreated.

### Clashes

1. **Text files** (UTF-8, under 5 MB, not binary): try a three-way line merge using
   the base as the common ancestor. If the two PCs edited different lines, both edits
   are kept and nobody is asked. This is the common case for code.
2. **Still clashing** (same lines edited, or a binary file): apply the project's policy.
   - **Keep both** (default): this PC's file stays. The server's arrives as
     `name (from Laptop 2026-10-26 2215).ext`. Both upload, so every PC ends up with
     both, and the Projects tab shows a badge until you delete the copy you don't want.
   - **Newest wins:** compare modification times. If they're within 2 minutes of each
     other, treat it as too close to call and use Keep both. The losing file goes to
     the trash, and the log says which one won and why.
   - **Ask me:** the clash waits in a **Clashes** list showing both versions (size,
     time, which PC, and a line diff for text). Choose **Keep mine**, **Take theirs**
     or **Keep both**. The rest of the project keeps syncing meanwhile.

### Git guard (git projects set to Sync)

Before touching a git project, compare the server manifest's `branch`/`head` with
this PC's.

- **Same branch and commit:** sync normally. Only your uncommitted edits differ, and
  that's exactly what's useful to carry over.
- **Different:** pause the project. The Projects tab says, for example, "Laptop is on
  `feature-x` @ `a1b2c3d`; this PC is on `main` @ `9f8e7d6`. Pull/push or switch
  branch, then sync." There's a **Sync anyway** button for when you know better.

This stops ClaudeSync merging the files of two different branches together.

## Restore and link flows

### A project this PC doesn't have yet

Projects tab → **On the server**: **Download to…** opens a folder picker,
pre-filled with your projects folder plus the project name. The files download, and
the folder is then linked, so it syncs from then on.

### A project this PC already has its own copy of (first link)

There's no base yet, so ClaudeSync shows a **preview** before changing anything:

```
Link "esp32_oled_clock" with the server copy from Laptop

  312 files identical
   14 only on this PC          → will upload
    9 only on the server       → will download
    3 different on both        → Keep both / Newest wins / Ask me   [choose]
    0 will be overwritten without a copy

  [ Merge ]   [ Use the server's copy ]   [ Use this PC's copy ]   [ Cancel ]
```

- **Merge** takes the union, with clashes handled as chosen.
- **Use the server's copy** makes this folder match the server. This PC's differing or
  extra files go to the trash first.
- **Use this PC's copy** uploads this folder as the new latest. Other PCs follow at
  their next sync, with their differing files going to trash.

### Ongoing

- **Automatic:** sync runs on the existing interval, projects first, then sessions, so
  when a chat restores, the code it refers to is already there.
- **Away from home:** an option limits project sync to your home network, or caps
  uploads per sync (for example 200 MB), so a big project doesn't upload over a phone
  hotspot.

## Always do this, or ask me

Every situation where ClaudeSync could guess wrong has a setting (Settings → Project folders):

| Situation | Choices | Default |
|---|---|---|
| Same file changed on two PCs | Keep both / Newest wins / Ask me (per project too) | Keep both |
| Linking a folder that already has files | Ask me (preview) / Always merge / Always use the server's copy / Always use this PC's copy | Ask me |
| A sync would remove many files (over 50, or over 20%) | Ask me / Always go ahead | Ask me |
| PCs on different git branches | Ask me / Always sync anyway / Never (wait) | Ask me |
| Files deleted on another PC | Move them to the trash here too / Ask me | Move to trash |
| New projects | Off / Back up only / Sync, with pushed git projects capped at Back up only | Off |
| Project over the size limit | Always asks once | |
| This PC's project folder is suddenly empty | **Always asks**: delete on other PCs too, or put the files back here | |

Questions show up on the folder's own card in Projects → **My folders**, with the buttons to answer them, and in a desktop notification. A project waiting for an answer doesn't
sync until you answer; nothing else is held up.

## Safety rules (same spirit as the rest of ClaudeSync)

1. **No file is replaced or removed without going to the local project trash first.**
   The trash is `<ClaudeSync data>/trash/<project>/<date>/<path>`, keeps 30 days or
   5 GB (oldest go first), and is restorable from the Recover tab, per file or a whole
   sync at once.
2. **Every sync uploads this PC's changes before applying anyone else's.**
3. **Only files whose content actually changed are written.** Unchanged files aren't
   touched, and mtimes are kept as they were on the PC that made the change.
4. **Writes are atomic** (write to a temporary name, then rename), so a crash never
   leaves half a file.
5. **Big changes ask first:** if a sync would put more than 20% of a project's files
   (or more than 50 files) in the trash, it pauses and shows the preview.
6. **Nothing is ever deleted on the server directly.** A version stops existing only
   when it is pruned past the 20 kept.

## Screens

- **Projects tab:** one row per project. Columns:
  - **Files:** Off / Back up only / Sync
  - **Clashes:** Keep both / Newest wins / Ask me
  - **Local folder**
  - **Size**
  - **Last synced**
  - **Status:** up to date / paused (git guard) / N clashes / N skipped files

  Also: an **Add folder…** button for folders with no Claude chats, and a
  **Default for new projects** switch (Off by default).
- **Clashes list:** both versions side by side, with **Keep mine / Take theirs / Keep both**.
- **Recover tab:** gains **Project trash**, with search and restore per file or per sync.
- **History tab:** projects appear like sessions, with versions. **Restore this version**
  writes that version into the folder (the current files go to trash first).

## The Projects tab (v0.5.0)

**My folders** is one card per folder:

- A coloured status (Up to date, Backed up, Syncing…, Needs you, Problem, Paused, Off), what it's doing
  right now ("Sending file 12 of 340"), when it was last checked, how many files and how big, and what the
  last change did ("3 sent to the server, 1 received").
- Questions (first link, clashes, deletions, git mismatch, too large) appear on the card itself, each with a
  plain explanation of what the buttons do.
- Errors are explained in plain words with **what to do**, with the raw error under "Technical details".
  A desktop notification is shown once per new problem or question, and the tray icon turns red meanwhile.
- **Sync now** syncs just that folder.
- **Details** has the folder's settings (Off / Back up only / Sync, clash choice, pause), a
  **Don't sync these** list (gitignore-style, this PC only), **Open folder**, **Change folder…**, files that were
  left out and why, a **Recent activity** log, and **Stop managing this folder**.

**Add folder…** looks at the folder first: file count and size, whether it's a git repository (and whether git
already carries it), what is skipped on purpose, files that look like passwords or keys, names Windows can't
have, files over the limit, and whether it is over the project size limit. Then you choose Back up only or Sync.

**Folders that are gone.** A folder that isn't on this PC any more (even one that's turned Off) shows a warning
and a one-click **Remove from list**. It only makes ClaudeSync forget the folder here: no files are deleted, the
server copy stays, and other PCs aren't affected.

**Folders whose chats are gone.** When the last chat that used a folder is deleted, the folder is removed from
My Folders (Settings → Project folders turns this off). Only this PC's list changes; files and the server copy are
never touched, and you get a notification. It is careful about it: the folder must have had a chat before, must not
have been added by hand with Add folder, nothing may be waiting on you, the sessions must have been read properly in
that sync, and it has to stay chat-less for at least five minutes across two checks.

Safety checks that explain themselves instead of failing quietly:

- A folder that **goes missing** (an unplugged drive, a rename) is never recreated empty: ClaudeSync stops with
  "The folder … isn't there" and nothing is changed or deleted anywhere. (A folder that is there but empty still
  asks before deleting anything elsewhere.)
- A folder can't be a whole drive, your home folder, ClaudeSync's own data, the same as another project, or
  inside or around another project (syncing both would copy files twice).
- "Don't sync these" never makes other PCs think those files were deleted: they are simply left alone on this PC.

## Build plan

| Phase | What you get | Risk |
|---|---|---|
| 1. Back up only | Chosen projects back up (chunked, encrypted, versioned); **Download a copy** into an empty folder; size and ignore rules | Low: never changes your existing folders |
| 2. Two-way sync | Base tracking, the table above, deletions to trash, Keep both clashes, first-link preview, project trash in Recover | Medium: this is where all the safety rules matter; built test-first with simulated PCs like the session tests |
| 3. Smarter merging | Three-way text merge, Newest wins, Ask me with diffs, git guard UI, home-network-only option | Medium |

Each phase is a separate release, and phase 2 isn't switched on for a project until you
pick **Sync** for it.

## Decisions made

1. New projects start Off until you pick them.
2. Keep both is the default clash rule.
3. Pushed git projects default to Back up only.
4. Limits: 200 MB per file, 2 GB per project (asks above), trash 30 days / 5 GB. All adjustable.
