# photo-organizer

Scan photo and video folders across multiple machines, find duplicates, and safely migrate or clean up files. Non-destructive — never moves or deletes files unless you explicitly run a generated script.

## Installation

```bash
make install        # install to ~/bin (no sudo)
make install-system # install to /usr/local/bin (requires sudo)
```

Make sure `~/bin` is in your PATH:
```bash
export PATH="$HOME/bin:$PATH"
```

## Workflow

### 1. Scan each machine

Run on every machine that has photos. Manifests land in `~/manifests/` automatically.

```bash
photo-organizer scan ~/Photos
photo-organizer scan /Volumes/ExternalDrive
photo-organizer scan /data/photos
```

### 2. Collect manifests

Copy all `~/manifests/_Manifest/*.csv` files from every machine to one place — Dropbox, USB drive, or any shared folder works.

### 3. Analyze

```bash
photo-organizer analyze                          # auto-loads ~/manifests/_Manifest/*.csv
photo-organizer analyze *.csv                    # explicit list
photo-organizer analyze *.csv --csv report       # also write CSV output files
```

The report shows:
- **Machine summaries** — file counts, total size, unique vs duplicated per machine
- **Stale manifest warnings** — sources not scanned in 30+ days
- **Duplicate groups** — files that exist on 2+ machines (sorted by size, confirmed vs unconfirmed)
- **Unique files** — files only on one machine (at risk if that machine fails)
- **Intra-machine duplicates** — same file in multiple folders, confirmed vs unconfirmed
- **Folder redundancy** — per-folder coverage %, flagging fully/nearly-redundant folders

### 4. Plan safe deletes

```bash
# Cross-machine: delete copies from other machines, keep on ubuntu
photo-organizer plan --keep ubuntu-max-acb605 --ssh ubuntu-max-acb605 --out cleanup.sh

# Intra-machine: delete duplicates within one machine
photo-organizer plan --intra Ls-MBP-967e82 --keep-under ~/Photos/Originals --out intra.sh

# Review (all rm commands are commented out), then run
bash cleanup.sh
```

### 5. Migrate unique files to another machine

```bash
photo-organizer migrate --from Ls-MBP-967e82 \
  --dest ubuntu@server:/tankm1/incoming \
  --out migrate.sh

bash migrate.sh   # safe to re-run — rsync skips completed files
```

### 6. Keep manifests fresh

```bash
photo-organizer rescan               # re-scan all previously scanned folders
photo-organizer rescan --prune       # also remove entries for deleted files
photo-organizer rescan --no-cache    # recompute all hashes (after algorithm change)
```

---

## Commands

### `scan [directory]`

```
photo-organizer scan [directory] [flags]

Flags:
  --root dir        write manifest to dir/_Manifest/  (default: ~/manifests)
  --machine name    machine label in manifest          (default: stable hardware ID)
  --full-hash       hash all files fully, not just colliding ones (rarely needed)
  --no-cache        recompute all hashes, ignoring cached values
  --prune           remove manifest entries for files no longer on disk
```

- Skips dot-folders, system dirs (`PRIVATE`, `THMBNL`, `AVF_INFO`, etc.), and symlinks (with warning)
- Caches results: repeat scans are near-instant when files haven't changed
- **Sampled hash**: reads first 32KB + last 32KB per file — captures both header and image data, dramatically reducing false collisions for RAW/HIF files from the same camera
- Automatically upgrades files with colliding partial hashes to full MD5
- Progress: shows current folder during walk, then X/Y processed
- Summary: files found, cached, new entries, hash upgrades, pruned, symlinks skipped, total size
- Backs up manifest before every write (keeps last 5 backups in `_backups/`)

### `rescan`

```
photo-organizer rescan [flags]

Flags:
  --machine id      rescan paths for this machine ID (default: current machine)
  --root dir        manifest directory (default: ~/manifests)
  --full-hash       hash all files fully, not just colliding ones
  --no-cache        recompute all hashes, ignoring cached values
  --prune           remove entries for deleted files (skips if >50% would be removed)
```

Reads all manifests in `~/manifests/_Manifest/`, finds unique scan paths for the current machine, and re-scans each one. Skips paths that no longer exist on disk.

**Prune safety**: if `--prune` would remove more than 50% of a manifest's entries (e.g. volume is unmounted), pruning is skipped with a warning for that path.

### `analyze [manifest...]`

```
photo-organizer analyze [manifest...] [flags]

Flags:
  --csv prefix      write CSV output files with this prefix
  --threshold n     folder coverage % to flag as nearly-redundant (default: 0.9)
```

Auto-loads `~/manifests/_Manifest/*.csv` when no manifests are specified.

- Detects overlapping parent/child scans — shared files are never falsely reported as duplicates
- Warns when any source hasn't been scanned in 30+ days
- Duplicate groups marked **CONFIRMED** (full hash match) vs **UNCONFIRMED** (partial hash only — re-scan to resolve)

### `plan --keep <machine> | --intra <machine>`

```
photo-organizer plan --keep <machine> [--ssh user@host] [--out file] [manifest...]
photo-organizer plan --intra <machine> [--keep-under /path] [--out file] [manifest...]

Flags:
  --keep machine      cross-machine: keep copies on this machine, delete from others
  --intra machine     intra-machine: deduplicate files within this machine
  --keep-under path   with --intra: keep copies under this path, delete others
  --ssh user@host     verify backup files exist on remote machine via SSH
  --out file          write script to file instead of stdout
```

Safety guarantees:
- **Cross-machine**: only files confirmed on the keep machine are included
- **Intra-machine**: only files with matching full hashes (confirmed identical) are included
- Backup paths are stat'd on disk (or via `--ssh`) — unverified paths flagged with ⚠
- All `rm` commands are **commented out** — review before running

### `migrate --from <machine> --dest <path>`

```
photo-organizer migrate --from <machine> --dest <dest-root> [--out file] [manifest...]

Flags:
  --from machine    source machine to copy unique files from (required)
  --dest path       destination root, e.g. user@host:/path or /local/path (required)
  --out file        write script to file instead of stdout
```

Generates a bash script using `rsync --partial --progress --files-from` to copy files unique to `--from` to `--dest`, preserving folder structure. Groups files by source scan path. Re-running is safe — rsync skips already-complete files.

---

## Machine identity

On first run, a stable machine ID is generated from `LocalHostName` + the first 6 characters of the hardware UUID:

```
Ls-MBP-967e82
ubuntu-server-acb605
```

Stored in `~/.photo-organizer-id` — stable across network changes, hostname renames, or Tailscale suffixes. Edit the file to rename a machine; use `--machine` to override for a single scan.

---

## Hashing

Files are identified by a **sampled partial hash**: MD5 of the first 32KB + last 32KB. This captures both the camera header and actual image data, making collisions rare even for RAW/HIF files from the same camera.

When two files share the same (partial hash, size), a **full-file MD5** is computed to confirm. The manifest stores both:

| Column | Description |
|--------|-------------|
| `partial_hash` | MD5 of first+last 32KB (always computed) |
| `full_hash` | MD5 of entire file (only when a collision is detected) |

In `analyze`, duplicate groups are marked:
- **CONFIRMED** — all copies have matching full hashes
- **UNCONFIRMED** — partial hash only; re-scan to resolve

---

## Manifest format

Manifests are CSV files in `~/manifests/_Manifest/` named `photo_manifest_<machine>_<path>.csv`.

| Column | Description |
|--------|-------------|
| `filename` | Base filename |
| `relative_path` | Path relative to scan root (unique key) |
| `file_size_bytes` | File size in bytes |
| `capture_date` | From EXIF, filename pattern, or mtime |
| `partial_hash` | Sampled MD5 (first+last 32KB) |
| `full_hash` | Full-file MD5 (empty if not needed) |
| `scan_date` | When this file was last scanned |
| `scan_path` | Absolute path of scan root |
| `machine_name` | Machine ID |

Manifests are append-only and backward-compatible with older formats.

---

## Supported file types

**Photos:** `.jpg` `.jpeg` `.png` `.gif` `.heic` `.hif` `.dng` `.arw` `.cr2` `.nef` `.raf`

**Videos:** `.mp4` `.mov` `.avi` `.mkv`

**Audio:** `.wav` `.mp3`

**Sidecars:** `.lrf` `.xmp` `.json`

---

## Building

```bash
make            # show help
make build      # build for current platform
make build-all  # build for Linux, macOS, Windows
make install    # install to ~/bin
make test       # run tests
```
