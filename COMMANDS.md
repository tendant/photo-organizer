# photo-organizer Commands Reference

Complete list of all available commands for backup, deduplication, and integrity verification.

## Core Workflow Commands

### scan
**Create a manifest of all files in a folder**

```bash
photo-organizer scan <path> [options]
```

- `<path>` - Directory to scan (required)
- `--machine-id NAME` - Custom machine identifier (default: hostname)
- `--manifest-path PATH` - Custom manifest location

**Output:** Creates manifest CSV at `~/manifests/_Manifest/<folder>-<date>.csv`

**Example:**
```bash
photo-organizer scan ~/Photos
photo-organizer scan ~/iPhone-Photos --machine-id iphone
```

---

### analyze
**Find duplicates across manifests**

```bash
photo-organizer analyze <manifest1.csv> [manifest2.csv ...]
```

- Takes one or more manifest files
- Shows duplicate groups (same size + partial hash)
- Reports total wasted space
- Identifies which machine has each copy

**Example:**
```bash
photo-organizer analyze ~/manifests/photos.csv
photo-organizer analyze photos-mac.csv photos-iphone.csv
```

---

### backup
**Copy files to timestamped archive**

```bash
photo-organizer backup <manifest-path> <archive-root>
```

- `<manifest-path>` - Manifest CSV to backup (required)
- `<archive-root>` - Directory to store archive (required)

**Creates:** `<archive-root>/YYYY-MM-DD-HHMMSS-name/` with all files

**Example:**
```bash
photo-organizer backup ~/manifests/photos.csv /mnt/backup
# Creates: /mnt/backup/2026-06-20-143022-Photos/
```

---

### restore
**Recover files from archive**

```bash
photo-organizer restore <archive-path> <destination>
```

- `<archive-path>` - Path to archive folder (required)
- `<destination>` - Where to restore files (required)

**Example:**
```bash
photo-organizer restore /mnt/backup/2026-06-20-143022-Photos ~/Recovered
```

---

### list-archives
**Show all available backups**

```bash
photo-organizer list-archives <archive-root>
```

Alias: `ls-archives`

- Lists all timestamped archive folders
- Shows file count and total size per archive
- Displays creation timestamp

**Example:**
```bash
photo-organizer list-archives /mnt/backup
```

---

## Data Integrity Commands

### verify-backup
**Check backup integrity and detect corruption**

```bash
photo-organizer verify-backup <manifest-path>
```

- Checks all files exist in archive
- Validates file sizes
- Detects missing/corrupted files
- Reports detailed statistics

**Example:**
```bash
photo-organizer verify-backup ~/manifests/photos.csv
```

---

### sign-manifest
**Create cryptographic signature for manifest**

```bash
photo-organizer sign-manifest <manifest-path> --key <secret-key>
```

- Creates HMAC-SHA256 signature
- Prevents accidental tampering
- Generates key fingerprint
- Outputs verification ID

**Example:**
```bash
photo-organizer sign-manifest ~/manifests/photos.csv --key "my-secret-key-123"
```

---

### repair-manifest
**Fix corrupted or invalid manifest entries**

```bash
photo-organizer repair-manifest <manifest-path> <archive-path>
```

- Removes entries for missing files
- Fixes file size mismatches
- Creates backup before modifying
- Safe rollback on failure

**Example:**
```bash
photo-organizer repair-manifest ~/manifests/photos.csv /mnt/backup/2026-06-20-143022-Photos
```

---

## Information Commands

### find-duplicates / dups / dup
**Find duplicate files (alias for analyze)**

```bash
photo-organizer find-duplicates [manifest1.csv ...]
photo-organizer dups manifest.csv
```

Same as `analyze` command.

---

### archive-status
**Show status of archive operations**

```bash
photo-organizer archive-status [path]
```

Shows archive creation times and metadata.

---

### backup-status
**Show backup operation status**

```bash
photo-organizer backup-status
```

Shows ongoing or recent backup operations.

---

### manifests
**List all manifests**

```bash
photo-organizer manifests
```

Shows all manifest files in `~/manifests/_Manifest/`

---

### machines
**List all machines that have been scanned**

```bash
photo-organizer machines
```

Shows which machines have contributed backups.

---

### stalled-manifests
**Show manifests with missing source directories**

```bash
photo-organizer stalled-manifests
```

Identifies manifests where the original scan path no longer exists.

---

## Management Commands

### archive
**Archive/move folders to timestamped location**

```bash
photo-organizer archive <source-folder>
```

Moves folder to archive location with timestamp.

---

### prune
**Remove manifest entries for deleted files**

```bash
photo-organizer prune <archive-path>
```

- Scans archive folder
- Removes entries for missing files
- Cleans up manifest

---

### check-backup
**Check if files have been backed up**

```bash
photo-organizer check-backup <folder>
```

Shows which files are missing from backups.

---

### verify
**Verify backed up files**

```bash
photo-organizer verify <manifest-path>
```

Comprehensive verification of manifest and archive.

---

### backup-missing
**Backup files that haven't been archived**

```bash
photo-organizer backup-missing <folder>
```

Identifies and backs up new/changed files.

---

### lookup
**Search for a specific file**

```bash
photo-organizer lookup <filename>
```

Finds file in manifests and shows all copies.

---

### remove-manifest
**Remove a manifest file**

```bash
photo-organizer remove-manifest <manifest-path>
```

Deletes manifest from tracking system.

---

### search
**Search for files matching pattern**

```bash
photo-organizer search <pattern>
```

Searches manifests for matching filenames.

---

## Utility Commands

### help / --help / -h
**Show help information**

```bash
photo-organizer help
photo-organizer --help
photo-organizer -h
```

---

### plan
**Show cleanup/organization plan**

```bash
photo-organizer plan <folder>
```

---

### collect
**Collect manifests from other machines**

```bash
photo-organizer collect [options]
```

---

### dup-folders
**Find duplicate folders**

```bash
photo-organizer dup-folders
```

---

## Command Categories by Use Case

### Daily Use
- `scan` - Create backup manifest
- `backup` - Back up to archive
- `verify-backup` - Check integrity
- `list-archives` - See what you've backed up

### Multi-Device Sync
- `scan` - Scan each device
- `analyze` - Find duplicates across devices
- `backup` - Back up unique files only

### Maintenance
- `verify-backup` - Check for corruption
- `repair-manifest` - Fix problems
- `prune` - Clean up deleted entries

### Security
- `sign-manifest` - Protect manifests
- `verify-backup` - Detect tampering

### Recovery
- `restore` - Get files back from archive
- `list-archives` - Find the right backup
- `lookup` - Find specific file

---

## Exit Codes

- `0` - Success
- `1` - Error/failure

---

## Configuration

Most commands use:
- `~/manifests/` - Default manifest directory
- `~/manifests/_Manifest/` - Manifest CSV files location
- `.photoignore` - File exclusion patterns (in scan folder)

---

## Tips

1. **Organize backups:** Use `list-archives` to manage multiple backups
2. **Verify regularly:** Run `verify-backup` after each backup
3. **Protect manifests:** Use `sign-manifest` for important backups
4. **Fix issues:** Use `repair-manifest` if corruption detected
5. **Recovery:** `restore` creates a copy; original archive unchanged
6. **Cross-device:** Scan each device, then `analyze` together for dedup

---

## Command Workflow

### Basic Backup
1. `scan ~/Photos`
2. `backup manifest.csv /mnt/archive`
3. `verify-backup manifest.csv`

### Multi-Device Backup
1. `scan ~/Photos --machine-id mac`
2. `scan ~/iPhone --machine-id iphone`
3. `analyze manifests/photos.csv manifests/iphone.csv`
4. `backup manifests/iphone.csv /mnt/archive`

### Recovery
1. `list-archives /mnt/archive`
2. `restore /mnt/archive/2026-06-20-143022-Photos ~/Recovery`

### Maintenance
1. `verify-backup manifests/photos.csv`
2. If issues: `repair-manifest manifests/photos.csv /mnt/archive/2026-06-20-143022-Photos`
3. `prune /mnt/archive`
