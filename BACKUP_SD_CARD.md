# Back Up An SD Card

## Basic Flow

```bash
photo-organizer scan /Volumes/CAMERA/DCIM --media-id camera-sd
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/backups/camera
photo-organizer collect
photo-organizer check-backup /Volumes/CAMERA/DCIM
```

## Local Destination

```bash
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest /mnt/external/camera
```

## Remote Destination

```bash
photo-organizer collect --add nas=user@nas.local
photo-organizer backup-missing /Volumes/CAMERA/DCIM --dest nas:/volume1/photos/camera
```

## Before Formatting The Card

Run:

```bash
photo-organizer check-backup /Volumes/CAMERA/DCIM
photo-organizer dups
```

Only format the card after the files you care about are represented in a verified backup location.

## Verify Archives

```bash
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
```
