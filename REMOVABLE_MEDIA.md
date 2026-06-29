# Removable Media

Use stable media IDs when scanning SD cards, external drives, or camera cards. This prevents the same device from appearing as a new source when the mount path changes.

## Scan A Card Or Drive

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd
photo-organizer scan /mnt/sdcard/DCIM --media-id travel-card
```

## Check Backup Coverage

```bash
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer storage-status
```

## Copy Missing Files

```bash
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/backups/camera
```

After copying, collect manifests and check again:

```bash
photo-organizer collect
photo-organizer check-backup /Volumes/CAMERA/DCIM
```

## Find Duplicates

```bash
photo-organizer dups
photo-organizer dup-folders --top 20
```

## Refresh A Scan

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd --prune
```

## Before Formatting Media

1. Scan the card with a stable `--media-id`.
2. Run `backup-missing` to copy missing files.
3. Run `collect`.
4. Run `check-backup` on the mounted card.
5. Optionally run `verify-archive` on the destination manifest.
