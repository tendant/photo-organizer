# Removable Media

Use stable media IDs when scanning SD cards, external drives, or camera cards. This prevents the same device from appearing as a new source when the mount path changes.

## How A Source Is Classified

All analysis commands (`check-backup`, `storage-status`, `storage-plan`, `dup-folders`, `manifests`, `backup-missing`) share one rule for deciding whether a manifest source is removable media or a durable device:

1. **machines.conf is authoritative.** A `[removable]` tag means removable; any other entry (an SSH target or `[local]`) means durable — even if the scan path looks like a removable mount (e.g. a NAS mounted under `/Volumes`).
2. **Unknown machines fall back to the scan path.** Paths under `/Volumes`, `/mnt`, `/media`, etc. are treated as removable.

Removable media never counts as a backup copy. To make sure a NAS or external RAID mounted under `/Volumes` is treated as durable, give it an entry in `machines.conf`.

## Scan A Card Or Drive

Use `--media-id` whenever the same card may mount at different paths:

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd
photo-organizer scan /mnt/sdcard/DCIM --media-id travel-card
```

The media ID is stored as the source identity in manifests. To refresh a device that was scanned before:

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd --prune
```

## Import And Back Up A Camera Card

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-card
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/backups/camera
photo-organizer collect
photo-organizer check-backup /Volumes/CAMERA/DCIM
```

The `--dest` target can be a local path or a configured remote machine:

```bash
# Local external drive
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest /mnt/external/camera

# Remote machine (register it first)
photo-organizer collect --add nas=user@nas.local
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/volume1/photos/camera
```

## Before Formatting Media

1. Scan the card with a stable `--media-id`.
2. Run `backup-missing` to copy missing files.
3. Run `collect`.
4. Run `check-backup` on the mounted card.
5. Optionally run `verify-archive` on the destination manifest.

## Review Duplicates And Archive

```bash
photo-organizer dups
photo-organizer dup-folders --top 20
photo-organizer dup-folders -s --min-devices 2   # folders safe to archive
```

After backup coverage is confirmed, archive local imports:

```bash
photo-organizer archive ~/Pictures/Imports/OldShoot
```

## Useful Commands

| Task | Command |
| --- | --- |
| Scan card | `photo-organizer scan <card-path> --media-id <id>` |
| Check backup coverage | `photo-organizer check-backup <card-path>` |
| Copy missing files | `photo-organizer backup-missing <card-path> --dest <target>` |
| Find duplicate files | `photo-organizer dups` |
| Find duplicate folders | `photo-organizer dup-folders --top 20` |
| Show storage status | `photo-organizer storage-status` |
| Show storage plan | `photo-organizer storage-plan` |
