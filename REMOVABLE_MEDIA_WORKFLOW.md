# Removable Media Workflow

## Import And Back Up A Camera Card

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-card
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/backups/camera
photo-organizer collect
photo-organizer check-backup /Volumes/CAMERA/DCIM
```

## Review Duplicates

```bash
photo-organizer dups
photo-organizer dup-folders --top 20
```

## Archive Local Imports

After backup coverage is confirmed:

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
| Show storage plan | `photo-organizer storage-plan` |
