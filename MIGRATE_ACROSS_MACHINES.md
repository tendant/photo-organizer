# Copy Missing Files Across Machines

The old generated migration-script workflow has been replaced by current backup commands.

## Recommended Workflow

1. Scan the source folder.

```bash
photo-organizer scan ~/Photos
```

2. Collect manifests from known machines.

```bash
photo-organizer collect
```

3. Check what is not backed up.

```bash
photo-organizer check-backup ~/Photos
photo-organizer storage-plan
```

4. Copy missing files to a destination.

```bash
photo-organizer backup ~/Photos --dest nas:/backups
```

5. Collect manifests again and verify.

```bash
photo-organizer collect
photo-organizer check-backup ~/Photos
```

## Destination Examples

```bash
photo-organizer backup ~/Photos --dest /mnt/archive/photos
photo-organizer backup ~/Photos --dest user@nas.local:/volume1/photos
photo-organizer backup /Volumes/CAMERA/DCIM --dest nas:/backups/camera
```

## Cleanup Review

Before deleting local copies, use folder-level duplicate and backup checks:

```bash
photo-organizer dup-folders --top 20
photo-organizer check-backup ~/Photos/OldImport
```

Prefer `archive` over immediate deletion:

```bash
photo-organizer archive ~/Photos/OldImport --dest /mnt/archive
```
