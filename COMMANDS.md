# photo-organizer Commands Reference

Current command surface for scanning, backing up, searching, and verifying photo collections.

## Core Workflow

### scan
Create or update a manifest for a folder.

```bash
photo-organizer scan <folder>
photo-organizer scan <folder> --machine <machine-id>
photo-organizer scan <folder> --root ~/manifests
photo-organizer scan <folder> --no-cache --prune
```

Common flags:
- `--machine <id>`: override the machine label stored in the manifest.
- `--root <dir>`: write manifests under `<dir>/_Manifest/`.
- `--media-id <id>`: stable ID for removable media.
- `--no-cache`: recompute hashes instead of reusing manifest cache.
- `--prune`: remove manifest entries for files no longer on disk.

### dups
Find duplicate files across collected manifests.

```bash
photo-organizer dups
photo-organizer dups ~/manifests/_Manifest/*.csv
photo-organizer dups --top 50
```

Use this when you want file-level duplicate groups and storage totals.

### dup-folders
Find duplicate or highly overlapping folders.

```bash
photo-organizer dup-folders
photo-organizer dup-folders --top 20
photo-organizer dup-folders -s
photo-organizer dup-folders -s --min-devices 2   # only folders safe to archive
```

Use this before manual cleanup; it is usually easier to review whole-folder overlap than individual files.

The summary shows a **Devices** count (distinct non-removable machines a folder lives on) and an **Archive?** verdict: a folder is only safe to archive when it exists on **2+ durable devices**, so archiving one copy still leaves redundancy. Removable media (SD cards, USB) is never counted as a device. Use `--min-devices N` to list only folders you can safely archive while staying on at least N devices, and `--min-copies N` to filter by raw copy count.

### organize
Copy scattered photos into a new date-sorted library. **Source folders are never
modified** — they are only read.

```bash
photo-organizer organize /data/Photos-All ~/Pictures --dest /tankm1/media/Library          # dry run
photo-organizer organize /data/Photos-All ~/Pictures --dest /tankm1/media/Library --apply
photo-organizer organize /mnt/usb/DCIM --dest ~/Library --layout month --apply

# Preview once, then copy exactly what you previewed
photo-organizer organize /data/Photos-All --dest ~/Library --save-plan ~/plan.json
photo-organizer organize --plan ~/plan.json            # re-check the plan, write nothing
photo-organizer organize --plan ~/plan.json --apply
```

Files land in `<library>/YYYY/YYYY-MM-DD/` (or `YYYY/YYYY-MM/` with
`--layout month`). The date comes from, in order of trust:

1. **EXIF** — JPEG, TIFF-based RAW (DNG, ARW, NEF, CR2...), and EXIF embedded in
   HEIC, CR3, RAF, ORF and RW2. The shutter time is preferred over the
   file-modified field.
2. **Video metadata** — the creation time in MP4/MOV/M4V/3GP. Phones write it in
   UTC, some cameras write local time; it is used as written.
3. **Filename** — `IMG_20240315_101010`, `DJI_...`, `2024-03-15`. A bare run of
   digits must stand alone, so counters like `IMG_120240315999` are not read as dates.
4. **File modified time** — last resort; copying or restoring a folder changes it.

The summary shows how many files were dated each way, warns about files that only
have a modified time, and warns when many files are dated January 1st (a camera
whose clock was never set — the tool cannot know the real date, so check those).
Implausible dates (before 1990, in the future) go to `undated/`.

- **Dry run by default.** Nothing is written until `--apply`.
- **Converges.** Content already in the library is skipped, so re-running after
  adding photos or after an interruption copies only what is missing.
- **Duplicates are confirmed, not guessed.** Size plus the scan's 64KB hash only
  nominates a candidate. Files up to 64 MB are then compared by a SHA-256 of every
  byte; larger files (videos) by SHA-256 of 16 pieces spread across the whole file,
  which is what keeps a multi-GB duplicate from costing a multi-GB read. The
  summary says how many duplicates were confirmed by sampling. A copy that differs
  only *between* the sampled pieces (a flipped bit, an in-place edit of the same
  length) would be missed; `--verify full` reads every byte of every file, and
  `--verify quick` samples anything over 1 MB.
- **Fast on re-runs.** Digests of library files are remembered in
  `<library>/_organize/index.csv`, keyed by size and modified time, so a file that
  has not changed is not read again. Only `--apply` writes it; a damaged or missing
  file is simply ignored. `--jobs N` (default 4) hashes N files at once; use
  `--jobs 1` when everything sits on one spinning disk.
- **Pairs stay together.** `IMG_1.CR3`, `IMG_1.JPG` and `IMG_1.xmp` (or
  `IMG_1.jpg.xmp`) share one date and folder. A sidecar always follows its own
  photo — even when two photos have identical sidecars, or the photo was already in
  the library (the sidecar is placed beside that copy). A sidecar that differs from
  the one already there is reported, never overwritten. Sidecars with no photo to
  attach to are ignored, as are empty files and macOS `._*` files.
- **Skips system folders.** `@eaDir`, `#recycle`, `.thumbnails`, `.Trash-*`,
  `$RECYCLE.BIN`, `*.lrdata` and similar are not photos and are not copied; the
  summary says which were skipped.
- **Tells you what it leaves behind.** Files that are not photos, videos or
  sidecars (`.pdf`, `.txt`, ...) are not copied. They are counted by extension in
  the summary — keep them before you retire or delete the source.
- **Safe to interrupt.** Ctrl-C (or SIGTERM) stops after discarding the file being
  copied, writes the log, and leaves only complete files; run the same command to
  continue. A second Ctrl-C quits immediately. If the process is killed outright, the
  leftover `.organize-tmp-*` files are reported by the next run and removed by
  `--apply` once they are over an hour old.
- **Checks free space** at the library before copying and refuses to start if the
  copy would not fit.
- **Never overwrites.** Same name with different content gets `_1`, `_2`...
  Files are written to a temporary name and linked into place.
- **Separate trees.** The library may not be inside a source, nor a source inside
  the library (symlinks are resolved before comparing).
- **Saved plans.** `--save-plan FILE` (preview only) records every copy. `--plan FILE
  --apply` then copies exactly those files, even if new photos appeared since. Before
  copying it re-checks: if a source file is gone or changed, or a target name is now
  taken by other content, nothing is copied and you are asked to preview again.
  Targets already holding the planned content (an interrupted earlier run) are
  skipped, so re-running the same command resumes. The plan file cannot be saved inside
  a source folder or the library; `--layout`, `--verify` and `--jobs` are fixed by the plan.
- A log of every copy goes to `<library>/_organize/`, and the library manifest is
  refreshed so the copies count for `dups` and `check-backup`.

Once the library holds everything, `dup-folders` and `check-backup` tell you
which of the old folders are safe to retire with `archive`.

### storage-status
Show storage breakdown by machine, device, and backup state.

```bash
photo-organizer storage-status
```

### storage-plan
Recommend what to back up next.

```bash
photo-organizer storage-plan
```

## Backup And Archive

`backup` and `archive` answer different questions:

| | `backup` | `archive` |
|---|---|---|
| Question | "does a second copy exist?" | "can this folder leave my library?" |
| Layout | mirrors the source tree | dated snapshot folder |
| Repeat runs | converge — copy only what is missing | each run makes a new snapshot |
| Source folder | untouched | moved (kept with `--keep`) |

Both accept a local path or a remote `machine-id:/path` or `user@host:/path`.

### backup
Keep a second copy of a folder somewhere else.

```bash
photo-organizer backup ~/Photos --dest nas:/backups
photo-organizer backup ~/Photos --dest ubuntu@192.168.1.100:/backups
photo-organizer backup ~/Photos --dest /Volumes/External
photo-organizer backup ~/Photos --dest nas:/backups --all
```

The folder is mirrored into a folder of the same name inside the target, so the
first example fills `nas:/backups/Photos`. That keeps one target usable for
several folders: backing up `~/Photos` and `~/email-export` to `nas:/backups`
gives `nas:/backups/Photos` and `nas:/backups/email-export`, rather than merging
both trees and letting same-named files overwrite each other.

To back a folder up to a specific name, name it in the target:
`--dest nas:/backups/photos-2026` writes to `nas:/backups/photos-2026/Photos`.

Repeated runs are safe to schedule: files already at the destination, and files
that already have a copy on another durable machine, are skipped. `--all` copies
everything regardless.

Afterwards the destination is scanned — over SSH for a remote target, in place
for a local one — so the new copies count for `dups`, `check-backup`, and the
next `backup` run.

Remote destinations require SSH key access and `photo-organizer` installed on the
remote machine. A `user@host` that is not in `machines.conf` still transfers the
files, but the manifest refresh is skipped — add it with
`photo-organizer collect --add <machine-id>=user@host` to make the copy count.

`backup-missing` is the former name of this command and still works, with a
deprecation notice.

#### Progress

Each slow phase — working out what needs copying, the copy itself, and the
destination rescan — reports percent done, amount done, throughput, and an ETA.
The copy is accounted in **bytes**, not file counts, because photo sizes range
from a few MB to several GB.

```
Checking which files need backup...
  Checking  38%  4,731 / 12,431 files  1,204 files/s  ETA 1m04s
Backing up 1,204 files (5.4 GB)...
  Copying  63%  3.4 GB / 5.4 GB  742 / 1,204 files  58.1 MB/s  ETA 35s
```

Suppress it with `--no-progress`, or set `PHOTO_ORGANIZER_PROGRESS=0`. When
stderr is not a terminal the same information is printed as ordinary lines every
few seconds rather than redrawn in place, so logs and cron mail stay readable.

Two things worth knowing about the numbers on a **remote** backup:

- The rate is **source bytes per second, not link throughput**. `rsync`
  compresses and reuses blocks it finds at the destination, so fewer bytes cross
  the wire than are counted here. The figure answers "how fast is my backup
  progressing", which is what the ETA needs.
- The file list is sent to `rsync` in byte-bounded batches so progress can move
  during the transfer. `rsync` still decides what actually needs sending and
  still does per-file delta transfer — batching changes only how progress is
  measured. A file large enough to be its own batch reports byte-level progress
  where `rsync` supports `--info=progress2`, and otherwise names the file it is
  sending so a slow-moving line is explained.

### archive
Retire a folder into a dated snapshot and repoint the manifests at it.

```bash
photo-organizer archive ~/Photos/OldImport --dest /mnt/archive
photo-organizer archive ~/Photos/OldImport --dest nas:/archive
photo-organizer archive ~/Photos/OldImport --dest /mnt/archive --keep
```

Creates a folder such as `/mnt/archive/2026-06-20-143022-OldImport/`. The
timestamp keeps every snapshot distinct, so a later run never overwrites an
earlier one.

A local archive moves the folder, then prunes the old manifest entries and scans
the new location. `--keep` copies instead, leaving the source in place. A remote
archive always copies and always keeps the source — deleting across the network
is never automatic. Verify with `check-backup`, then remove the source yourself.

### restore
Restore files from an archive snapshot.

```bash
photo-organizer restore /mnt/archive/2026-06-20-143022-OldImport ~/Recovered
```

### list
List available timestamped archives.

```bash
photo-organizer list /mnt/archive
```

### verify-archive
Verify archive integrity against its manifest.

```bash
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
```

### sign
Sign a manifest with HMAC-SHA256.

```bash
photo-organizer sign ~/manifests/_Manifest/photos.csv --key "secret-key"
```

### fix
Repair a manifest/archive mismatch.

```bash
photo-organizer fix ~/manifests/_Manifest/photos.csv /mnt/archive/2026-06-20-143022-Photos
```

## Search And Inventory

### search
Search manifest contents.

```bash
photo-organizer search -name 'IMG_*'
photo-organizer search -path '2024'
photo-organizer search -hash abc123
photo-organizer search -size '100MB-500MB'
photo-organizer search -date 2024-07-01:2024-07-31
photo-organizer search -machine laptop
photo-organizer search -duplicates-only -group
photo-organizer search -csv results.csv
```

### lookup
Find one item and show its full manifest details.

```bash
photo-organizer lookup IMG_001
photo-organizer lookup vacation/beach.jpg
```

### manifests
List scanned folders and manifest status.

```bash
photo-organizer manifests
photo-organizer manifests --stalled
photo-organizer manifests --cleanup
photo-organizer manifests --remove ~/Photos/OldImport
```

### machines
List machines represented in manifests.

```bash
photo-organizer machines
```

## Remote Manifest Sync

### collect
Pull manifests from configured machines.

```bash
photo-organizer collect --add nas=user@nas.local
photo-organizer collect --list
photo-organizer collect
photo-organizer collect --from nas
photo-organizer collect --from nas --sync-delete
```

Machine config is stored at `~/manifests/machines.conf`.

## Cleanup Helpers

### check-backup
Check whether a folder is backed up and where copies exist.

```bash
photo-organizer check-backup ~/Photos
```
