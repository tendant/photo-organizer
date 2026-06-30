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
```

Use this before manual cleanup; it is usually easier to review whole-folder overlap than individual files.

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

## Backup And Restore

### backup
Back up a folder to a timestamped archive.

```bash
photo-organizer backup ~/Photos /mnt/archive
photo-organizer backup ~/Photos /mnt/archive --new-only
```

Creates an archive folder such as `/mnt/archive/2026-06-20-143022-Photos/`.

### backup-missing
Copy only files that are not already represented in collected manifests.

```bash
photo-organizer backup-missing ~/Photos --dest nas:/backups/photos
photo-organizer backup-missing ~/Photos --dest user@host:/backups/photos
```

### restore
Restore files from an archive.

```bash
photo-organizer restore /mnt/archive/2026-06-20-143022-Photos ~/Recovered
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

### archive
Move a folder to a timestamped local archive location.

```bash
photo-organizer archive ~/Photos/OldImport
```

### check-backup
Check whether a folder is backed up and where copies exist.

```bash
photo-organizer check-backup ~/Photos
```
