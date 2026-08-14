# Usage Guide

Practical workflows for scanning, backing up, checking coverage, and finding duplicates.

## 1. Scan And Collect Manifests

Scan each photo source:

```bash
photo-organizer scan ~/Photos
photo-organizer scan /Volumes/Camera/DCIM --media-id camera-sd
```

Register remote machines once:

```bash
photo-organizer collect --add laptop=lei@laptop.local
photo-organizer collect --add nas=lei@nas.local
```

Pull remote manifests:

```bash
photo-organizer collect
photo-organizer collect --from nas
```

## 2. Find Duplicate Files

Use `dups` for file-level duplicate groups:

```bash
photo-organizer dups
photo-organizer dups ~/manifests/_Manifest/*.csv
photo-organizer dups --top 50
```

This shows duplicate groups, machine locations, and reclaimable space.

## 3. Find Duplicate Folders

Use `dup-folders` when you want cleanup candidates that are easier to review:

```bash
photo-organizer dup-folders
photo-organizer dup-folders --top 20
photo-organizer dup-folders -s
```

Review the output manually before deleting anything. The tool reports overlap; it does not automatically remove files.

## 4. Back Up A Folder

Create a timestamped archive, on a local path or on another machine:

```bash
photo-organizer backup ~/Photos /mnt/archive
photo-organizer backup ~/Photos /mnt/archive --new-only
photo-organizer backup ~/Photos nas:/backups/photos
```

A remote archive root (`machine-id:/path` or `user@host:/path`) transfers over
SSH with `rsync`, then rescans the archive on the remote machine so the copy
counts as a real backup.

Back up only files that are missing from known manifests:

```bash
photo-organizer backup-missing ~/Photos --dest nas:/backups/photos
```

## 5. Check Backup Coverage

Before cleanup, check whether a folder is backed up:

```bash
photo-organizer check-backup ~/Photos
```

For a broader view of storage and risk:

```bash
photo-organizer storage-status
photo-organizer storage-plan
```

## 6. Verify And Repair Archives

Verify an archive manifest:

```bash
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
```

Sign important manifests:

```bash
photo-organizer sign ~/manifests/_Manifest/photos.csv --key "secret-key"
```

Repair manifest/archive mismatches:

```bash
photo-organizer fix ~/manifests/_Manifest/photos.csv /mnt/archive/2026-06-20-143022-Photos
```

## 7. Search Across Manifests

```bash
photo-organizer search -name 'IMG_*'
photo-organizer search -path '2024'
photo-organizer search -hash abc123def456
photo-organizer search -size '100MB-500MB'
photo-organizer search -date 2024-07-01:2024-07-31
photo-organizer search -machine laptop
photo-organizer search -duplicates-only -group
photo-organizer search -csv results.csv
```

Use `lookup` for detailed information about one file:

```bash
photo-organizer lookup IMG_001
```

## 8. Manage Manifests

List known scanned folders:

```bash
photo-organizer manifests
photo-organizer manifests --stalled
```

Clean up stale manifest records interactively:

```bash
photo-organizer manifests --cleanup
```

Remove a specific scanned folder from manifest tracking:

```bash
photo-organizer manifests --remove ~/Photos/OldImport
```

## 9. Work With Archives

Move a local folder into a timestamped archive:

```bash
photo-organizer archive ~/Photos/OldImport
```

List archive contents:

```bash
photo-organizer list /mnt/archive
```

Restore files:

```bash
photo-organizer restore /mnt/archive/2026-06-20-143022-Photos ~/Recovered
```

## 10. Handle Slow SSH Connections

Remote checks use `PHOTO_ORGANIZER_SSH_TIMEOUT` when a longer timeout is needed:

```bash
export PHOTO_ORGANIZER_SSH_TIMEOUT=120s
photo-organizer collect --from nas
```
