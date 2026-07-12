# Removable Media Tracking

Photo Organizer tracks removable media best when each card or drive has a stable ID.

## How A Source Is Classified

All analysis commands (`check-backup`, `storage-status`, `storage-plan`, `dup-folders`, `manifests`, `backup-missing`) share one rule for deciding whether a manifest source is removable media or a durable device:

1. **machines.conf is authoritative.** A `[removable]` tag means removable; any other entry (an SSH target or `[local]`) means durable — even if the scan path looks like a removable mount (e.g. a NAS mounted under `/Volumes`).
2. **Unknown machines fall back to the scan path.** Paths under `/Volumes`, `/mnt`, `/media`, etc. are treated as removable.

Removable media never counts as a backup copy. To make sure a NAS or external RAID mounted under `/Volumes` is treated as durable, give it an entry in `machines.conf`.

## Stable IDs

Use `--media-id` whenever the same card may mount at different paths:

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id sony-a7iv-card-1
photo-organizer scan /Volumes/TRAVEL/DCIM --media-id travel-sd-card
```

The media ID is stored as the source identity in manifests.

## Refreshing A Device

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id sony-a7iv-card-1 --prune
```

## Backup State

```bash
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer storage-status
photo-organizer storage-plan
```

## Duplicate Review

```bash
photo-organizer dups
photo-organizer dup-folders --top 20
```
