# Removable Media Tracking

Photo Organizer tracks removable media best when each card or drive has a stable ID.

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
