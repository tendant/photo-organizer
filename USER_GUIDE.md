# photo-organizer User Guide

Complete guide to backing up, organizing, and verifying photo collections.

## Quick Start

### 1. Scan Your Photos

```bash
photo-organizer scan ~/Photos
```

This creates a manifest CSV file containing:
- File names and sizes
- Capture dates (from EXIF)
- Partial hashes (first 1KB for quick detection)
- Full hashes (computed on-demand for duplicates)

Output: `~/manifests/_Manifest/photos-2026-06-20.csv`

### 2. Find Duplicates

```bash
photo-organizer analyze ~/manifests/photos-2026-06-20.csv
```

Shows:
- Duplicate groups (same size + partial hash)
- Total storage wasted
- Files that can be safely deleted
- Which machine has each copy

### 3. Back Up to Archive

```bash
photo-organizer backup ~/Photos /mnt/archive
```

Copies unique files to archive with timestamped folders:
- `/mnt/archive/2026-06-20-143022-Photos/` contains all files
- Manifest updated with archive location
- No duplicates copied

### 4. Verify Backup Integrity

```bash
photo-organizer verify-backup ~/manifests/photos-2026-06-20.csv
```

Checks:
- All files exist in archive
- File sizes match manifest
- No corruption detected
- Generates integrity report

---

## Core Concepts

### Manifests

A manifest is a CSV file listing all files from a scan:

```csv
file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2026-06-19,1048576,abc123...,def456...,/Users/lei/Photos,macbook,vacation/photo1.jpg
2026-06-19,2097152,xyz789...,uvw012...,/Users/lei/Photos,macbook,vacation/photo2.jpg
```

**Key fields:**
- `file_modified`: Date photo was modified
- `file_size_bytes`: File size (used for duplicate detection)
- `partial_hash`: MD5 of first 1KB (quick comparison)
- `full_hash`: Full MD5 (only computed for duplicates)
- `scan_path`: Where the file was scanned from
- `machine_name`: Which machine this came from
- `relative_path`: Path relative to scan root

### Duplicates

Two files are considered duplicates if:
1. Same file size
2. Same partial hash (first 1KB)

This is fast and accurate for photos (collision probability < 1 in billions).

### Archives

An archive is a backup location containing:
- Physical files (copies of originals)
- Timestamped folder: `YYYY-MM-DD-HHMMSS-name/`
- Linked to manifest for verification

Example:
```
/mnt/archive/
  2026-06-20-143022-Photos/
    vacation/
      photo1.jpg
      photo2.jpg
    family/
      reunion.jpg
```

---

## Commands Reference

### scan

Scan a folder and create a manifest.

```bash
photo-organizer scan ~/Photos
```

**Options:**
- `--machine-id NAME` - Custom machine identifier (default: hostname)
- `--manifest-path PATH` - Custom manifest location (default: ~/manifests/_Manifest/)

**What happens:**
1. Walks entire directory tree
2. Skips dotfiles and system folders (`.DS_Store`, `.stfolder/`, etc.)
3. Respects `.photoignore` patterns
4. Computes file sizes and partial hashes
5. Extracts capture dates from EXIF
6. Writes manifest CSV

**Output:** `~/manifests/_Manifest/photos-YYYY-MM-DD.csv`

### analyze

Compare manifests and find duplicates.

```bash
photo-organizer analyze manifest1.csv manifest2.csv [manifest3.csv ...]
```

**Output shows:**
- Duplicate groups (files with same content)
- Storage wasted by duplicates
- Which machine has each copy
- Which files can be safely deleted

**Example output:**
```
Duplicate Group #1: 2.3 MB
  /Users/lei/Photos/vacation/beach.jpg (macbook)
  /Users/alice/Photos/shared/beach.jpg (iphone)

Total wasted: 15.2 GB across 324 duplicates
```

### backup

Copy unique files to archive, deduplicating across manifests.

```bash
photo-organizer backup manifest.csv /mnt/archive
```

**What happens:**
1. Reads all manifests you've scanned
2. Groups files by hash (finds duplicates)
3. Copies only unique files to archive
4. Creates timestamped folder
5. Skips files already in archive (by hash)
6. Updates manifest with archive location

**Archive structure:**
```
/mnt/archive/
  2026-06-20-143022-Photos/  ← Timestamped folder
    vacation/photo1.jpg
    vacation/photo2.jpg
    family/reunion.jpg
```

### verify-backup

Check backup integrity and detect corruption.

```bash
photo-organizer verify-backup manifest.csv
```

**Checks:**
- All files exist in archive
- File sizes match manifest
- Spot-checks with partial hashes
- Detects missing/corrupted files

**Output:**
```
Archive: /mnt/archive/2026-06-20-143022-Photos
Verified: 2,847 files
Corrupted: 0 files
Missing: 0 files
Status: ✓ Healthy
```

### sign-manifest

Cryptographically sign a manifest for integrity verification.

```bash
photo-organizer sign-manifest manifest.csv --key "my-secret-key"
```

**Creates:**
- HMAC-SHA256 signature
- Key fingerprint for tracking
- Verification ID

**Use case:** Ensure manifests haven't been accidentally modified.

### repair-manifest

Remove corrupted entries and fix issues.

```bash
photo-organizer repair-manifest manifest.csv /path/to/archive
```

**Repairs:**
- Removes entries for missing files
- Fixes file size mismatches
- Removes invalid entries (negative sizes, empty paths)
- Creates backup before modifying

**Output:**
```
Removed: 23 invalid entries
Fixed: 5 size mismatches
Backup: manifest.csv.backup.2026-06-20-143022
```

---

## Real-World Workflows

### Scenario 1: Back Up Photos from Multiple Devices

**Day 1: Back up macbook**
```bash
photo-organizer scan ~/Photos --machine-id macbook
photo-organizer backup ~/manifests/_Manifest/photos-2026-06-20.csv /mnt/archive
```

**Day 2: Back up iPhone**
```bash
photo-organizer scan ~/iPhone-Photos --machine-id iphone
# iPhone photos automatically deduplicated against macbook's
photo-organizer analyze ~/manifests/_Manifest/photos-2026-06-20.csv ~/manifests/_Manifest/iphone-photos-2026-06-21.csv
photo-organizer backup ~/manifests/_Manifest/iphone-photos-2026-06-21.csv /mnt/archive
```

Result: One copy of each unique photo, even if same photo is on both devices.

### Scenario 2: Verify Backup Integrity

```bash
# After backup sits for a month, verify nothing corrupted
photo-organizer verify-backup ~/manifests/_Manifest/photos-2026-06-20.csv

# Check multiple backups
photo-organizer verify-backup manifest1.csv manifest2.csv manifest3.csv
```

### Scenario 3: Fix Corrupted Manifest

```bash
# Manifest references files that don't exist (accidental deletion)
photo-organizer repair-manifest ~/manifests/_Manifest/photos-2026-06-20.csv /mnt/archive/2026-06-20-143022-Photos

# Backup created: photos-2026-06-20.csv.backup.2026-06-20-150000
# Invalid entries removed, manifest cleaned up
```

### Scenario 4: Organize with .photoignore

```bash
# Don't back up working files, only final exports
cat > ~/Photos/.photoignore <<'EOF'
.DS_Store
.claude/
Lightroom/
_temp/
*.lnk
EOF

photo-organizer scan ~/Photos
# Only includes files not matching .photoignore patterns
```

---

## FAQ

**Q: How much storage will a backup need?**
A: Approximately the size of all your unique files. Duplicates are not copied.

**Q: What if I delete a photo after backing it up?**
A: The manifest entry remains. Use `prune` to clean up old entries for deleted files.

**Q: Can I back up to multiple locations?**
A: Yes. Each backup can be to a different archive path. Use separate manifest copies if needed.

**Q: How do I restore files from a backup?**
A: The backup folder contains all original files in their original structure. Copy files back to restore.

**Q: What if the backup folder gets corrupted?**
A: Use `verify-backup` to detect corruption, then `repair-manifest` to remove entries for corrupted files. Re-backup those files.

**Q: Can I use this for video files?**
A: Yes! Works with any file type. Same deduplication logic applies.

**Q: How long does scanning take?**
A: ~1-2 files per second depending on file size and disk speed. Hashing 1KB per file for partial hash. Full hashes only computed when duplicates detected.

**Q: Is this secure?**
A: Manifests can be signed with HMAC-SHA256. Use `sign-manifest` to verify integrity. Backup files are standard files - use OS-level encryption if needed.

---

## Troubleshooting

**Issue: Scan reports "permission denied"**
- Solution: Some files may be locked. Scan will skip them and continue. Re-run to try again.

**Issue: Duplicate detection finds false positives**
- Solution: Rare. Partial hash collision. Full hash computed on demand will distinguish.

**Issue: Manifest file is very large**
- Solution: This is normal. 1 million files = ~500MB manifest. Consider splitting by date or folder.

**Issue: Backup is slow**
- Solution: Check disk speed. Network backups are slower. Consider local backup first, then sync.

**Issue: Verify-backup reports missing files**
- Solution: Use `repair-manifest` to remove entries for deleted files, then re-backup.

---

## Best Practices

1. **Scan regularly** - Capture new changes in fresh manifests
2. **Keep manifests** - Archive manifest files alongside backups for verification
3. **Test restores** - Verify you can actually restore files from backup
4. **Use .photoignore** - Exclude work-in-progress files, temporary data
5. **Sign important manifests** - Use `sign-manifest` for critical backups
6. **Verify backups** - Run `verify-backup` after backups complete
7. **Multiple copies** - Back up to multiple locations for redundancy
8. **Document your setup** - Note which machine backed up which data

---

## Next Steps

- Read [.photoignore guide](PHOTOIGNORE.md) for exclusion patterns
- See [Data Integrity](INTEGRITY.md) for security features
- Check [Architecture](ARCHITECTURE.md) for technical details
