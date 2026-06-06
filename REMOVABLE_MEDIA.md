# Handling Removable Media (SD Cards, USB Drives, etc)

## Current Workflow

### 1. Scan When Mounted

**Mount your SD card**:
```bash
# macOS: usually auto-mounts as /Volumes/SDCARD
# Linux: mount /dev/sdX /mnt/sdcard
# Windows: via Windows Explorer
```

**Scan it**:
```bash
photo-organizer scan /Volumes/SDCARD -machine "camera-1"
```

**Result**: Creates manifest in `~/manifests/_Manifest/photo_manifest_camera-1_*.csv`

---

### 2. Best Practices for SD Cards

**Use machine name to track source**:
```bash
# Same camera, different scan = same machine
photo-organizer scan /Volumes/SDCARD -machine "canon-5d"
# Later, after emptying and refilling...
photo-organizer scan /Volumes/SDCARD -machine "canon-5d"
```

**Why**: Manifests are merged by machine name. Same machine + same scan path = automatic deduplication.

**Add to machines config** (optional):
```bash
photo-organizer collect --add camera-1=lei@localhost
```

---

### 3. Common Workflow: "Clean Up After Import"

**Scenario**: Import photos from camera, find duplicates before deleting from card.

```bash
# 1. Mount SD card
mount /dev/sdX /mnt/sdcard

# 2. Scan it
photo-organizer scan /mnt/sdcard -machine "canon-eos"

# 3. Find duplicates with your laptop
photo-organizer analyze

# 4. Are there duplicates? Show them
photo-organizer search -machine "canon-eos" -duplicates-only

# 5. If duplicates exist on laptop, safe to delete from card
photo-organizer plan --keep laptop
bash _Quarantine_*.sh  # Remove duplicates

# 6. If all unique, copy to backup
photo-organizer migrate --from canon-eos --dest /Volumes/backup
bash _migrate_*.sh

# 7. Now safe to format SD card and put it back in camera
```

---

### 4. Handling Disconnected Media

**Problem**: SD card unmounts or gets disconnected

**Solution**: Manifests persist—just remount and rescan:
```bash
# Card was disconnected, now reconnected
mount /dev/sdX /mnt/sdcard

# Rescan to update (adds new files, removes deleted ones)
photo-organizer rescan

# Continue with analysis/cleanup as before
```

---

### 5. Multiple SD Cards from Same Camera

**Scenario**: You have 3 SD cards for your Canon, want to see all photos across them.

```bash
# Card 1
mount /dev/sdX1 /mnt/sd1
photo-organizer scan /mnt/sd1 -machine "canon-eos-card1"

# Card 2
mount /dev/sdX2 /mnt/sd2
photo-organizer scan /mnt/sd2 -machine "canon-eos-card2"

# Card 3
mount /dev/sdX3 /mnt/sd3
photo-organizer scan /mnt/sd3 -machine "canon-eos-card3"

# Find duplicates across all cards
photo-organizer analyze

# Show all duplicates from any card
photo-organizer search -machine "canon-eos-*" -duplicates-only
```

**Note**: This treats them as separate machines, which is useful if you want to track which card has which files.

**Alternative** (if you want to treat as one camera):
```bash
# Use same machine name but different scan paths
photo-organizer scan /mnt/sd1 -machine "canon-eos"
photo-organizer scan /mnt/sd2 -machine "canon-eos"
photo-organizer scan /mnt/sd3 -machine "canon-eos"

# Now "canon-eos @ /mnt/sd1", "canon-eos @ /mnt/sd2", etc.
photo-organizer machines  # Shows all scan paths
```

---

### 6. Avoid Re-scanning Same Card

**Problem**: Every time you mount the card, it rescans everything (slow).

**Solution**: Use checkpoint system

```bash
# First scan (slow, hashes everything)
photo-organizer scan /mnt/sdcard -machine "camera"

# Later, if you just deleted some files and want fresh scan
photo-organizer rescan  # Skips unchanged files via cache
```

**Cache location**: `~/.photo-organizer-hash-cache`

**Force full rescan** (if cache is wrong):
```bash
photo-organizer scan /mnt/sdcard --no-cache
```

---

### 7. Handle "Camera Detected" on Startup

**macOS example**:
```bash
#!/bin/bash
# Run when SD card auto-mounts
MOUNT_POINT=$1
MACHINE_NAME="canon-eos"

photo-organizer scan "$MOUNT_POINT" -machine "$MACHINE_NAME"
echo "Scan complete: $MOUNT_POINT"
```

Save as `~/bin/scan-sdcard.sh` and configure auto-run on mount.

---

## Recommended Folder Structure

**On your laptop**:
```
~/manifests/
├── _Manifest/
│   ├── photo_manifest_laptop_Users_lei_Photos.csv
│   ├── photo_manifest_canon-eos_...csv
│   ├── photo_manifest_phone_...csv
│   └── photo_manifest_backup-nas_...csv
└── machines.conf
```

**SD Card organization**:
```
/mnt/sdcard/
├── DCIM/
│   └── 100CANON/          # Keep camera folder structure
│       ├── IMG_0001.JPG
│       ├── IMG_0002.JPG
│       └── ...
└── PHOTO_ORGANIZER_MANIFEST.csv  (optional, for reference)
```

---

## Workflow Patterns

### Pattern 1: "Ingest & Archive"
```bash
# 1. Mount camera
# 2. Scan it
photo-organizer scan /mnt/camera -machine "camera"
# 3. Copy unique files to archive
photo-organizer migrate --from camera --dest /Volumes/archive
# 4. Compare with laptop
photo-organizer plan --keep laptop
# 5. Safe to format card
```

### Pattern 2: "Multi-Camera Backup"
```bash
# Scan all cameras once
for cam in canon nikon sony; do
  mount /dev/sdX /mnt/$cam
  photo-organizer scan /mnt/$cam -machine "$cam"
done

# Find duplicates across all
photo-organizer analyze

# Backup unique from each
for cam in canon nikon sony; do
  photo-organizer migrate --from $cam --dest /backups
done
```

### Pattern 3: "Weekly Import"
```bash
# Weekly: check SD card against laptop
photo-organizer scan /mnt/camera -machine "camera"
photo-organizer search -machine "camera" -duplicates-only
photo-organizer migrate --from camera --dest /backups
# Then safely wipe card
```

---

## Limitations & Workarounds

### Issue: "SD card keeps changing, manifests become stale"
**Solution**: Rescan before analysis:
```bash
photo-organizer rescan --machine camera
photo-organizer analyze
```

### Issue: "Multiple cards with overlapping photos"
**Solution**: Use unique machine names to track source:
```bash
photo-organizer scan /mnt/sd1 -machine "camera-sd1"
photo-organizer scan /mnt/sd2 -machine "camera-sd2"
photo-organizer search -machine "camera-*" -duplicates-only
```

### Issue: "Don't want to create manifest on card itself"
**Solution**: Manifests are always in `~/manifests/_Manifest/` on your computer.
Only the source files are on the card—manifests track them remotely.

### Issue: "Card might be stolen/lost, want backup of its manifest"
**Solution**: Commit manifests to git:
```bash
cd ~/manifests
git init
git add _Manifest/*.csv
git commit -m "Backup manifests"
git remote add origin git@server:manifests.git
git push
```

---

## Future Enhancements (Possible)

1. **Auto-scan on mount**: Detect SD card, scan automatically
2. **Format-safe erase**: Erase only confirmed duplicate files from card
3. **Cloud sync**: Sync manifests to cloud (Google Drive, S3)
4. **Offline mode**: Work with cached manifests when card unmounted
5. **Card profiling**: Remember which files came from which card for future reference

