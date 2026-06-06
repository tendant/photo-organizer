# Managing Removable Media with Machines

Removable media (SD cards, external drives, USB sticks) are managed using the same **machine concept** as any other data source. No special "media" command needed — just use `scan` with a descriptive machine name.

## Simple Workflow

### Step 1: Insert and Scan Removable Media

```bash
# Insert SD card, it mounts at /Volumes/Sony-Card

# Scan it with a descriptive name
photo-organizer scan /Volumes/Sony-Card -machine "Sony-A7IV-Travel"

# Output shows:
# File Type Coverage Report: /Volumes/Sony-Card
# Files to be SCANNED: 1,905 files (160.8 GB)
# Proceeding with scan...
# ✓ Scan complete
```

The machine name becomes the permanent record of where the files came from.

### Step 2: Check What You Have

```bash
# See all machines (including removable media)
photo-organizer machines

# Output shows:
# Sony-A7IV-Travel
#   Last scanned: 2026-06-06 14:30:15
#   Files: 1,905  (160.8 GB)
#   Scan paths:
#     /Volumes/Sony-Card
```

### Step 3: Backup to Different Machines

```bash
# Backup to your main backup drive
photo-organizer migrate --from Sony-A7IV-Travel --dest /mnt/backup

# Backup to NAS
photo-organizer migrate --from Sony-A7IV-Travel --dest backup-nas:/photos

# Backup to cloud
photo-organizer migrate --from Sony-A7IV-Travel --dest backup-cloud:/photos
```

Each generates a self-contained script that can resume if interrupted.

### Step 4: Verify and Clean Up

```bash
# View analysis
photo-organizer analyze

# Shows coverage:
# Sony-A7IV-Travel: 1,905 files
# backup-nas: 1,905 files (same files)
# backup-cloud: 1,905 files (same files)

# Safe to reformat the card
# The files are on 3 different machines now
```

---

## Real-World Examples

### Scenario 1: Travel Photography Workflow

You have a Sony camera and back it up to multiple locations:

```bash
# Day 1: Return from trip, insert SD card
photo-organizer scan /Volumes/Sony-Card -machine "Sony-A7IV-2026-05"

# Backup to 3 locations
photo-organizer migrate --from Sony-A7IV-2026-05 --dest /external-ssd
bash _migrate_Sony_A7IV_2026_05_1.sh

photo-organizer migrate --from Sony-A7IV-2026-05 --dest nas:/photos
bash _migrate_Sony_A7IV_2026_05_2.sh

photo-organizer migrate --from Sony-A7IV-2026-05 --dest cloud:/backup
bash _migrate_Sony_A7IV_2026_05_3.sh

# Verify all backups
photo-organizer analyze
# Shows: Sony-A7IV-2026-05 files on 3 machines ✓

# Safe to reformat card
```

### Scenario 2: Multiple SD Cards

Organize by which card was inserted:

```bash
# Card 1: Professional shoot
photo-organizer scan /Volumes/SanDisk-Pro -machine "Shoot-Pro-NYC-June2026"

# Card 2: Travel backup
photo-organizer scan /Volumes/Samsung-512G -machine "Backup-Card-Travel"

# View all
photo-organizer machines
# Shows both cards with their metadata

# Backup each independently
photo-organizer migrate --from Shoot-Pro-NYC-June2026 --dest /archive
photo-organizer migrate --from Backup-Card-Travel --dest /archive
```

### Scenario 3: External Drive Rotation

Track which external drives you're using for backups:

```bash
# Drive 1
photo-organizer scan /mnt/external-1 -machine "WD-4TB-Rotation-Slot-1"

# Drive 2
photo-organizer scan /mnt/external-2 -machine "WD-4TB-Rotation-Slot-2"

# View which drives were scanned when
photo-organizer machines

# Find which drive has specific files
photo-organizer search -machine WD-4TB-Rotation-Slot-1

# Check if a drive is missing
photo-organizer machines
# Shows last scan date — alerts you if a drive hasn't been seen in months
```

---

## Machine Naming Convention

Choose names that describe **what** the media is and **when** it was scanned:

### Good Names:
- `Sony-A7IV-Travel` — Camera + purpose
- `Shoot-NYC-June-2026` — Location + date
- `Backup-Card-1` — Function + identifier
- `iPhone-2026-05-backup` — Device + date
- `DJI-Drone-2026-Q2` — Equipment + period

### Avoid:
- `card1` — Too generic, meaningless later
- `backup` — Multiple backups need different names
- `USB-20260606-042057` — Timestamp makes it hard to remember

---

## Tracking Removable Media Health

Since machines track last scan date, you can spot missing media:

```bash
# List all machines with scan dates
photo-organizer machines

# Manual check: which drives haven't been scanned in >30 days?
photo-organizer machines | grep -B2 "Last scanned: 2026-0[1-4]"
# Shows older scans that might be missing or forgotten
```

If an old machine's files are only on that removable media:
- Risk: Media lost or corrupted
- Solution: Rescan it and verify backup is complete
- Or: Move files to non-removable machine for safety

---

## Operations You Can Do

| Operation | Command |
|-----------|---------|
| Register media | `scan /path -machine "name"` |
| View all media | `machines` |
| Find files on specific media | `search -machine "name"` |
| Backup to another machine | `migrate --from "name" --dest /path` |
| Check duplication status | `analyze` |
| Find at-risk files | `risk-report` |

Everything works with the machine concept. No special commands needed.

---

## Why This Works

**Machine = Any data source**
- Your laptop's /Photos folder → machine "MacBook-Pro"
- SD card in your camera → machine "Sony-A7IV-Travel"
- External backup drive → machine "WD-4TB-Backup"
- NAS → machine "backup-nas"

**One system handles all:**
- Duplicate detection across all machines
- Risk analysis (which files are at-risk)
- Migration (copy unique files)
- Backup verification

No separate workflow for "removable" vs "permanent" — just machines with metadata.

---

## Quick Reference

```bash
# Scan removable media
photo-organizer scan /mount/point -machine "descriptive-name"

# See what was scanned
photo-organizer machines

# Backup to other machines (one at a time)
photo-organizer migrate --from source-machine --dest /backup
bash _migrate_source_machine_1.sh

photo-organizer migrate --from source-machine --dest nas:/backup  
bash _migrate_source_machine_2.sh

# Verify backups
photo-organizer analyze

# Clean media once verified
# Unmount and reformat safely
```

That's it! One concept, one workflow.
