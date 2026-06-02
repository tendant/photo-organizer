# photo-organizer

Scan photo and video folders across multiple machines, find duplicates, and generate safe-delete plans. Non-destructive — never moves or deletes files.

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
- **Machine summaries** — file counts, total size, unique vs duplicated
- **Duplicate groups** — files that exist on 2+ machines (sorted by size)
- **Unique files** — files only on one machine (at risk if that machine fails)
- **Intra-machine duplicates** — same file in multiple folders on one machine, with paths
- **Folder redundancy** — per-folder coverage %, flagging fully/nearly-redundant folders

### 4. Plan safe deletes

```bash
photo-organizer plan --keep nas-main             # what can be removed from other machines
photo-organizer plan --keep nas-main --out cleanup.sh
```

Generates a shell script of `rm` commands for files confirmed to exist on the `--keep` machine. Backup paths are verified on disk where accessible. All commands are **commented out** — review before running.

## Commands

### `scan [directory]`

```
photo-organizer scan [directory] [flags]

Flags:
  --root dir        write manifest to dir/_Manifest/  (default: ~/manifests)
  --machine name    machine label in manifest          (default: stable hardware ID)
  --full-hash       full-file hash for colliding files (default: first 64KB only)
```

- Skips dot-folders, system dirs (`PRIVATE`, `THMBNL`, `AVF_INFO`, etc.), and symlinks (with warning)
- Caches results: repeat scans are near-instant when files haven't changed
- Captures date from EXIF → filename patterns → file modification time
- Progress: shows current folder during walk, then X/Y processed
- Summary: files found, cached, new, upgraded, symlinks skipped, total size

### `analyze [manifest...]`

```
photo-organizer analyze [manifest...] [flags]

Flags:
  --csv prefix      write CSV output files with this prefix
  --threshold n     folder coverage % to flag as nearly-redundant (default: 0.9)
```

Auto-loads `~/manifests/_Manifest/*.csv` when no manifests are specified.

Detects and handles overlapping parent/child scans on the same machine — shared files are never falsely reported as duplicates.

### `plan --keep <machine> [manifest...]`

```
photo-organizer plan --keep <machine> [manifest...] [flags]

Flags:
  --keep machine    authoritative machine to keep (required)
  --out file        write script to file instead of stdout
```

Auto-loads `~/manifests/_Manifest/*.csv` when no manifests are specified.

Safety guarantees:
- Files unique to any machine are never included
- Backup paths are stat'd on disk — unverified paths are flagged with ⚠
- All `rm` commands are commented out — nothing executes automatically

## Machine identity

On first run, a stable machine ID is generated from `LocalHostName` + the first 6 characters of the hardware UUID:

```
Ls-MBP-967e82
ubuntu-server-acb605
```

Stored in `~/.photo-organizer-id` and reused on every scan — stable across network changes, hostname renames, or Tailscale suffixes. Edit the file to rename a machine; use `--machine` to override for a single scan.

## Manifest format

Manifests are CSV files in `~/manifests/_Manifest/` named `photo_manifest_<machine>_<path>.csv`. One row per file:

| Column | Description |
|--------|-------------|
| `filename` | Base filename |
| `relative_path` | Path relative to scan root (unique key) |
| `file_size_bytes` | File size in bytes |
| `capture_date` | From EXIF, filename pattern, or mtime |
| `file_hash` | MD5 of first 64KB (or full file if collision) |
| `hash_mode` | `partial` or `full` |
| `scan_path` | Absolute path of scan root |
| `machine_name` | Machine ID |

Manifests are append-only and backward-compatible with older formats.

## Supported file types

**Photos:** `.jpg` `.jpeg` `.png` `.gif` `.heic` `.hif` `.dng` `.arw` `.cr2` `.nef` `.raf`

**Videos:** `.mp4` `.mov` `.avi` `.mkv`

**Audio:** `.wav` `.mp3`

**Sidecars:** `.lrf` `.xmp` `.json`

## Building

```bash
make            # show help
make build      # build for current platform
make build-all  # build for Linux, macOS, Windows
make install    # install to ~/bin
```
