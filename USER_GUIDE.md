# photo-organizer User Guide

Photo Organizer is a command-line tool for building manifests of photo folders, collecting those manifests across machines, finding duplicates, and verifying backups.

## Quick Start

### 1. Scan Photos

```bash
photo-organizer scan ~/Photos
```

This writes a CSV manifest under `~/manifests/_Manifest/` with relative paths, sizes, timestamps, media metadata, and hashes.

### 2. Find Duplicates

```bash
photo-organizer dups
photo-organizer dup-folders --top 20
```

Use `dups` for file-level duplicate groups. Use `dup-folders` when deciding which folders are cleanup candidates.

### 3. Back Up

```bash
photo-organizer backup ~/Photos --dest nas:/backups/photos
photo-organizer backup ~/Photos --dest /Volumes/External/photos
```

`backup` mirrors the folder into the destination, copying only files that are missing there or not yet backed up on another durable machine. The destination may be a local path or a remote machine (`machine-id:/path` or `user@host:/path`, copied over SSH). Use `archive` instead when you want a dated snapshot you keep untouched.

### 4. Verify

```bash
photo-organizer check-backup ~/Photos
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
```

Use `check-backup` before cleanup and `verify-archive` to detect missing or corrupted archive files.

## Core Concepts

### Manifests

A manifest is a CSV inventory of a scanned folder. Important columns include:

- `relative_path`: file path relative to the scan root.
- `file_size_bytes`: exact file size.
- `partial_hash`: sampled hash used with size for duplicate detection.
- `full_hash`: full-file hash when available.
- `scan_path`: folder that was scanned.
- `machine_name`: machine or media ID that produced the scan.

### Duplicate Detection

Duplicate grouping uses file size plus sampled hash. This is fast for large photo libraries and avoids relying on filenames.

### Archives

Archives are ordinary folders with timestamped names, for example:

```text
/mnt/archive/
  2026-06-20-143022-Photos/
    vacation/photo1.jpg
    family/reunion.jpg
```

The archive can be checked later with `verify-archive`.

## Common Workflows

### Multiple Machines

```bash
photo-organizer collect --add laptop=lei@laptop.local
photo-organizer collect --add nas=lei@nas.local

ssh lei@laptop.local "photo-organizer scan ~/Photos"
ssh lei@nas.local "photo-organizer scan /volume1/photos"

photo-organizer collect
photo-organizer dups
photo-organizer storage-status
```

### Removable Media

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd
photo-organizer check-backup /Volumes/CAMERA/DCIM
```

Stable media IDs prevent a removable device from looking like a new source every time its mount path changes.

### Cleanup Review

```bash
photo-organizer dup-folders --top 20
photo-organizer check-backup ~/Photos/OldImport
photo-organizer archive ~/Photos/OldImport --dest /mnt/archive
```

The recommended cleanup workflow is review first, verify backup coverage, then archive or manually delete.

### Search

```bash
photo-organizer search -name 'IMG_*'
photo-organizer search -date 2024-07-01:2024-07-31
photo-organizer search -size '500MB-'
photo-organizer search -duplicates-only -group
photo-organizer lookup IMG_001
```

### Integrity

```bash
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
photo-organizer sign ~/manifests/_Manifest/photos.csv --key "secret-key"
photo-organizer fix ~/manifests/_Manifest/photos.csv /mnt/archive/2026-06-20-143022-Photos
```

`sign` protects manifests from accidental tampering. `fix` removes invalid or missing archive entries after verification finds problems.

## FAQ

**How much storage will a backup need?**
Approximately the size of files not already represented in known manifests.

**Can I back up videos too?**
Yes. The scanner includes common photo, video, audio, and sidecar formats.

**What should I do before deleting duplicates?**
Run `check-backup` on the folder and prefer archiving first with `archive` so recovery is straightforward.

**What if a manifest references files that no longer exist?**
Use `manifests --stalled`, `manifests --cleanup`, or `fix` depending on whether the issue is a missing source folder or a bad archive manifest.

**How do I avoid scanning temporary files?**
Add a `.photoignore` file to the scan tree. See `PHOTOIGNORE.md`.

## Best Practices

1. Scan after imports or major reorganizations.
2. Collect remote manifests before duplicate or backup analysis.
3. Use stable machine and media IDs.
4. Run `check-backup` before cleanup.
5. Verify archives periodically with `verify-archive`.
6. Keep manifests with your archives.
7. Use `.photoignore` for temporary, generated, or sync-system files.
