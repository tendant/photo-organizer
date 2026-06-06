# Tracking Removable Media Across Machines

When you plug an SD card into different machines, you need a way to track it as the **same physical media** — not as different machines. The `--media-id` flag solves this.

## The Problem

Without a stable ID, the same SD card gets different machine names on different machines:

```bash
# Machine A (January)
photo-organizer scan /Volumes/Sony-Card -machine "sony-camera"
# Creates manifest: sony-camera @ /Volumes/Sony-Card

# Machine B (February)  
photo-organizer scan /Volumes/Sony-Card -machine "sony-camera"
# Creates another manifest: sony-camera @ /Volumes/Sony-Card

# Problem: analyze shows "duplicates" when it's the same card!
photo-organizer analyze
# Shows files appear on "sony-camera" twice (confusing)
```

## The Solution: Use --media-id

Use `--media-id` to assign a **stable identifier** for removable media:

```bash
# Machine A - first time scanning this SD card
photo-organizer scan /Volumes/Sony-Card -machine "work-pc" --media-id "sony-a7iv-card-1"
# Creates: sony-a7iv-card-1 @ /Volumes/Sony-Card

# Machine B - same card inserted
photo-organizer scan /Volumes/Sony-Card -machine "laptop" --media-id "sony-a7iv-card-1"
# Creates: sony-a7iv-card-1 @ /Volumes/Sony-Card (SAME machine name!)

# Now analyze knows it's the same card
photo-organizer analyze
# Shows files are on "sony-a7iv-card-1" from both work-pc and laptop
# (You know it's the same card moving between machines)
```

## How It Works

When you use `--media-id`:
- The machine name is **replaced with the media ID**
- `--machine` is ignored (media ID takes precedence)
- The manifest is created with the stable media ID name
- Multiple scans of the same media use the same machine name

```bash
# These all create the same machine "travel-sd-card":
photo-organizer scan /Volumes/Card -machine "work-pc" --media-id "travel-sd-card"
photo-organizer scan /Volumes/Card -machine "laptop" --media-id "travel-sd-card"
photo-organizer scan /Volumes/Card -machine "server" --media-id "travel-sd-card"
```

## Naming Convention

Choose a stable name that describes the media, not the current location:

**Good names:**
- `sony-a7iv-card-1` — Camera + card number
- `travel-backup-usb` — Purpose + device type
- `external-4tb-backup` — Device + use
- `phone-backup-may2026` — Device + period

**Avoid:**
- `work-pc-scan` — Changes based on which machine scans it
- `usb-001` — Meaningless identifier
- Dates alone — Card is used across multiple dates

## Workflow: Multi-Machine Removable Media

### Scenario: Travel Photographer with Multiple Backups

```bash
# At home: Insert travel SD card into Work PC
photo-organizer scan /Volumes/Travel-Card -machine "work-pc" --media-id "travel-sd-card"

# At office: Insert same SD card into Laptop
photo-organizer scan /Volumes/Travel-Card -machine "laptop" --media-id "travel-sd-card"

# At backup NAS: Insert same SD card into NAS USB reader
photo-organizer scan /mnt/card-reader -machine "nas" --media-id "travel-sd-card"

# Verify all scans are tracked as the same card
photo-organizer machines
# Shows:
# travel-sd-card
#   Last scanned: 2026-06-06 14:30:15
#   Files: 5,432  (85.2 GB)
#   Scan paths:
#     /Volumes/Travel-Card (work-pc)
#     /Volumes/Travel-Card (laptop)
#     /mnt/card-reader (nas)
```

### Scenario: Multiple SD Cards

If you have multiple SD cards for the same camera:

```bash
# Card 1
photo-organizer scan /Volumes/Sony-Card1 --media-id "sony-a7iv-card-1"

# Card 2
photo-organizer scan /Volumes/Sony-Card2 --media-id "sony-a7iv-card-2"

# Card 3
photo-organizer scan /Volumes/Sony-Card3 --media-id "sony-a7iv-card-3"

# Each card has its own stable identity
photo-organizer machines
# Shows:
# sony-a7iv-card-1
# sony-a7iv-card-2
# sony-a7iv-card-3
```

## Detecting Double-Scanning

If you accidentally scan the same card twice without using `--media-id`, you can detect it:

```bash
# Two separate machine entries with identical files
photo-organizer search -duplicates-only
# Shows files from both "sony-camera" and "sony-camera-2"

# Check if they're the same physical card
photo-organizer machines
# If scan paths look similar and timestamps are close, it's likely the same card
```

**To fix:** Delete one manifest and use `--media-id` going forward:

```bash
rm ~/manifests/_Manifest/photo_manifest_sony-camera-2_*.csv
photo-organizer scan /Volumes/Sony-Card --media-id "sony-a7iv-card"
```

## Best Practices

✅ **Assign a media-id when first scanned:**
```bash
photo-organizer scan /Volumes/NewCard --media-id "new-travel-card"
```

✅ **Always use the same media-id for that card:**
```bash
# Later, on a different machine
photo-organizer scan /Volumes/NewCard --media-id "new-travel-card"
```

✅ **Use --media-id ONLY for removable media:**
```bash
# For removable media:
photo-organizer scan /Volumes/ExternalDrive --media-id "backup-drive-1"

# For permanent machines:
photo-organizer scan ~/Photos  # No media-id needed
```

✅ **Document your card assignments:**
```bash
# In ~/manifests/media-registry (optional reference file)
sony-a7iv-card-1=SanDisk Pro 64GB
sony-a7iv-card-2=Samsung EVO 128GB
travel-backup=WD 4TB External
```

## Workflow with analyze

Once you use `--media-id`, `analyze` shows exactly what you need:

```bash
photo-organizer analyze

Machines in manifests:
  travel-sd-card
    Files: 5,432  (85.2 GB)
    Backup: backup-nas
    Files: 5,432  (85.2 GB)  [DUPLICATE]
    
  backup-drive-1
    Files: 12,000  (240 GB)
    Backup: —
```

Interpretation:
- `travel-sd-card` is backed up to `backup-nas` (good!)
- `backup-drive-1` has no backup yet (at risk!)

## Migration to --media-id

If you have existing manifests without media IDs:

```bash
# List existing machines
photo-organizer machines

# Identify removable media (machines with /Volumes/, /mnt/, etc.)

# For each one, rescan with --media-id
photo-organizer rescan travel-sd-card --media-id "travel-sd-card"

# Or if you know the path:
photo-organizer scan /Volumes/TravelCard --media-id "travel-sd-card"
```

## Summary

Use `--media-id` when:
- Scanning external drives, SD cards, USB sticks
- You might scan the same media on different computers
- You want to track a specific piece of hardware as it moves between machines

Don't use `--media-id` for:
- Your computer's internal drives (use `--machine` instead)
- Permanently mounted network shares
- Backup destinations (use `--machine` to identify the backup machine)

This ensures each piece of physical media has one identity in your photo-organizer system!
