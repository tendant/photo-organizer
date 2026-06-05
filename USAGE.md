# Usage Guide

## Workflows & Examples

### 1. Organize Photos Across Multiple Machines

**Scenario**: You have photos on laptop, phone, and camera—want to find duplicates and keep one authoritative copy.

**Setup**:
```bash
# Add your machines
photo-organizer collect --add laptop=lei@laptop.local
photo-organizer collect --add phone=lei@phone.local
photo-organizer collect --add camera=lei@camera.local
```

**Scan each machine**:
```bash
ssh lei@laptop.local "photo-organizer scan ~/Photos"
ssh lei@phone.local "photo-organizer scan ~/DCIM"
ssh lei@camera.local "photo-organizer scan /mnt/sdcard"

# Collect manifests to central location
photo-organizer collect
```

**Find duplicates**:
```bash
photo-organizer analyze
```

**Generate cleanup plan** (keep only laptop copy):
```bash
photo-organizer plan --keep laptop
```

**Review script** (all deletions are commented out):
```bash
cat _Quarantine_*.sh
```

**Run cleanup** (manually uncomment rm lines first):
```bash
bash _Quarantine_*.sh
```

---

### 2. Verify Backups Before Cleanup

**Scenario**: Want to delete duplicates only after confirming backup copies exist on NAS.

```bash
# Generate plan with SSH verification
photo-organizer plan --keep laptop --ssh nas:/backups/photos

# Output shows:
# ✓ /backup/photos/IMG_001.jpg (verified on disk)
# ✗ /backup/photos/IMG_002.jpg (NOT FOUND)

# Only uncomment rm lines for verified backups!
```

---

### 3. Find & Organize by Date

**Scenario**: Identify all photos from a specific date range.

```bash
# Find all photos from July 2024
photo-organizer search -date 2024-07-01:2024-07-31

# Show as groups (see all copies together)
photo-organizer search -date 2024-07-01:2024-07-31 -group

# Export for review
photo-organizer search -date 2024-07-01:2024-07-31 -csv july-2024.csv
```

---

### 4. Large-Scale Migration

**Scenario**: Migrate unique files (those only on one machine) to backup NAS.

```bash
# Generate migration script
photo-organizer migrate --from laptop --dest nas:/volume1/backups

# Review generated script
less _migrate_*.sh

# Run migration (resumable if interrupted!)
bash _migrate_*.sh

# Check progress
ls ~/manifests/_migrate/*/group_*.done | wc -l
```

**Resume after interruption**:
```bash
# Just run the same script again—completed groups skip automatically
bash _migrate_*.sh
```

**Force full re-run** (if needed):
```bash
rm ~/manifests/_migrate/*/group_*.done
bash _migrate_*.sh
```

---

### 5. Deduplication Within One Machine

**Scenario**: Laptop has folders `/Photos` and `/Photos-Archive`—find duplicates within laptop only.

```bash
# Plan intra-machine deduplication
photo-organizer plan --intra laptop --keep-under /Photos

# Output shows which copies to keep under /Photos, which to delete from /Photos-Archive
```

---

### 6. Find Duplicates Across Specific Machines

**Scenario**: Find only files that are duplicated between laptop and NAS (ignore phone).

```bash
# Scan only laptop and NAS manifests
photo-organizer analyze ~/manifests/_Manifest/*laptop* ~/manifests/_Manifest/*nas*

# Generate plan keeping NAS copy
photo-organizer plan --keep nas ~/manifests/_Manifest/*laptop* ~/manifests/_Manifest/*nas*
```

---

### 7. Search by Size (Find Large Duplicates)

**Scenario**: Find large video duplicates eating up space.

```bash
# Find duplicate videos > 500MB
photo-organizer search -name '*.mp4' -size '500MB-' -duplicates-only -group

# See all copies together
photo-organizer search -name '*.mp4' -size '500MB-' -group
```

---

### 8. Find Duplicate by Hash

**Scenario**: You know the hash of a file (from a previous search) and want to find all copies.

```bash
# Find all copies of this file
photo-organizer search -hash abc123def456

# See them grouped
photo-organizer search -hash abc123def456 -group
```

---

### 9. Handle SSH Timeouts on Slow Networks

**Scenario**: Your NAS is on a slow remote connection, SSH verification times out.

```bash
# Increase SSH timeout from 30s to 60s
photo-organizer plan --keep laptop --ssh nas:/backups --ssh-timeout 60s

# Or set environment variable for all operations
export PHOTO_ORGANIZER_SSH_TIMEOUT=120s
photo-organizer plan --keep laptop --ssh nas:/backups
```

---

### 10. Recover from Interrupted Scan

**Scenario**: Large scan was interrupted—resume from checkpoint.

**Startup message** shows:
```
⚠  Found 1 incomplete scan(s). Run rescan to resume:
   photo-organizer rescan  (to resume scan-manifest.csv)
```

**Resume**:
```bash
photo-organizer rescan
# Automatically resumes from last checkpoint
```

---

## Command Reference

### scan
```bash
photo-organizer scan [directory]              # Scan current or specified dir
photo-organizer scan --machine custom-name    # Override machine name
photo-organizer scan --auto-identify-folders  # Auto-detect photo folders
photo-organizer scan --score-threshold 40     # Higher threshold = fewer folders
photo-organizer scan --detect-only            # Preview detection without scanning
photo-organizer scan --no-cache                # Recompute all hashes
```

### analyze
```bash
photo-organizer analyze                       # Auto-load manifests from ~/manifests/_Manifest/
photo-organizer analyze a.csv b.csv           # Analyze specific manifests
photo-organizer analyze --csv report          # Export analysis to CSV
photo-organizer analyze --threshold 0.95      # Flag folders >95% covered
```

### plan
```bash
photo-organizer plan --keep laptop            # Keep on laptop, move others to quarantine
photo-organizer plan --intra laptop           # Dedup within one machine
photo-organizer plan --keep-under /Photos     # With --intra, keep copies under path
photo-organizer plan --ssh nas:/backups       # Verify backups via SSH
photo-organizer plan --ssh-timeout 60s        # Custom SSH timeout
photo-organizer plan --delete                 # Generate rm instead of mv
photo-organizer plan --out script.sh          # Write to file instead of stdout
```

### migrate
```bash
photo-organizer migrate --from laptop --dest /local/backup
photo-organizer migrate --from laptop --dest nas:/volume1/backups
photo-organizer migrate --from laptop --dest s3://bucket/photos
photo-organizer migrate --out migration.sh    # Write script to file
```

### search
```bash
photo-organizer search                        # Load all manifests
photo-organizer search a.csv b.csv            # Search specific manifests
photo-organizer search -name 'IMG_*'          # Glob pattern
photo-organizer search -path '2024'           # Path substring
photo-organizer search -hash abc123           # By hash
photo-organizer search -size '100MB-500MB'    # Size range
photo-organizer search -size '1GB-'           # 1GB or larger
photo-organizer search -date 2024-07-01       # Exact date
photo-organizer search -date 2024-07-01:2024-07-31  # Date range
photo-organizer search -machine laptop        # Filter by machine
photo-organizer search -duplicates-only       # Only files with >1 copy
photo-organizer search -group                 # Group by hash
photo-organizer search -csv results.csv       # Export to CSV
```

### collect
```bash
photo-organizer collect                       # Collect from all configured machines
photo-organizer collect --from laptop         # Collect from specific machine
photo-organizer collect --add nas=user@nas    # Register machine
photo-organizer collect --list                # Show registered machines
```

### rescan
```bash
photo-organizer rescan                        # Rescan all folders on this machine
photo-organizer rescan --machine laptop       # Rescan specific machine
photo-organizer rescan --prune                # Remove entries for deleted files
```

### machines
```bash
photo-organizer machines                      # List all machines in manifests
```

### risk-report
```bash
photo-organizer risk-report                   # Find files at risk
```

---

## Tips & Best Practices

1. **Always preview before deleting**: Review the generated script before uncommenting rm lines
2. **Verify backups**: Use `--ssh` to confirm backup files exist before cleanup
3. **Start small**: Test on a small set of duplicates first
4. **Keep manifests**: Don't delete CSV files—they're your backup record
5. **Use groups**: `-group` shows all copies together, makes decisions easier
6. **Check disk space**: Large migrations show available space in pre-flight checks
7. **Resume interrupted ops**: Just run the same command again—completed groups skip
8. **Export for review**: Use `-csv` to export results for spreadsheet review
