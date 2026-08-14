# API & Developer Documentation

## Project Structure

```
photo-organizer/
├── main.go              # CLI dispatch, usage, and command routing
├── analyze.go           # Shared duplicate/search/compliance helpers
├── duplicate_analysis.go # Duplicate index, reports, CSV exports, dups command
├── storage.go           # Storage status, storage plan, and check-backup commands
├── commands_scan.go     # Scan command
├── commands_collect.go  # Remote manifest/config collection commands
├── commands_dup_folders.go # Duplicate-folder command
├── manifest_read.go     # Manifest CSV loading and validation
├── checkpoint.go        # Scan checkpoint storage and startup hints
├── config_paths.go      # Shared manifest/config path helpers
├── machines_config.go   # Machine listing, removable detection, config IO
├── commands_backup.go   # Backup command (mirror a folder to another location)
├── commands_archive.go  # Archive, restore, and archive listing commands
├── backup_helpers.go    # Shared destination, transfer, and manifest-refresh helpers
├── commands_integrity.go # Manifest/archive integrity commands
├── integrity.go         # Integrity verification helpers
├── photoignore.go       # .photoignore parsing
├── main_test.go         # Tests for scanning, hashing, manifests
├── analyze_test.go      # Tests for analysis, validation, edge cases
├── README.md            # Quick start
├── USAGE.md             # Workflow examples
└── TROUBLESHOOTING.md   # Solutions to common issues
```

## Data Structures

### ManifestRow
```go
type ManifestRow struct {
    Filename      string    // Original filename (for display)
    RelativePath  string    // Relative to scan root
    SizeBytes     int64     // File size in bytes
    PartialHash   string    // MD5 of first + last 32KB
    FullHash      string    // MD5 of entire file (for duplicates)
    Extension     string    // File extension
    ScanDate      string    // When scanned (YYYY-MM-DD)
    ScanPath      string    // Absolute path scanned
    MachineName   string    // Source machine ID
}
```

### ManifestSource
```go
type ManifestSource struct {
    FilePath    string          // CSV file path
    MachineName string          // Machine ID from manifest
    ScanPath    string          // Root path that was scanned
    Label       string          // Display label (machine @ path)
    LastScanned string          // Most recent scan date
    Rows        []ManifestRow   // All file entries
}
```

### DuplicateGroup
```go
type DuplicateGroup struct {
    PartialHash string   // Hash of files
    FullHash    string   // Non-empty if confirmed via full hash
    SizeBytes   int64    // File size
    Locations   []string // "label: relative_path" for each copy
    Confirmed   bool     // True if full_hash populated
}
```

## CSV Manifest Format

**File**: `~/manifests/_Manifest/photo_manifest_<machine>_<path>.csv`

**Columns** (order matters, headers required):
```
filename | relative_path | file_size_bytes | file_size_mb | file_modified | 
capture_date | camera_make | camera_model | partial_hash | full_hash | 
extension | scan_date | scan_path | machine_name
```

**Example row**:
```
IMG_001.jpg,Photos/2024/IMG_001.jpg,2000000,1.9,2024-07-15 10:30:00,
2024:07:15 10:30:00,,,,abc123def456,xyz789uvw012,.jpg,2024-07-15,/data/Photos,laptop
```

**Notes**:
- All fields required (use empty string for unknown)
- `partial_hash`: MD5 of first 32KB + last 32KB
- `full_hash`: MD5 of entire file (empty until full hash computed)
- `scan_date`: When file was scanned (used for manifest staleness)
- Can include old formats (fallback to `file_hash` for older manifests)

## Key Algorithms

### Sampled Hashing (Fast Duplicate Detection)

1. Read first 32KB of file
2. Read last 32KB of file (if file > 32KB)
3. Hash concatenation: MD5(first + last) → `partial_hash`
4. On collision: compute full MD5 → `full_hash`

**Why**: 99.9% accuracy with 1000x faster scanning than full hash.

**Trade-off**: 1 in 10,000 false negatives (files with same 64KB samples).

### Duplicate Detection

1. Build hash index: `hash → [file1, file2, file3, ...]`
2. Entries with 2+ files are duplicates
3. Same hash on different machines = confirmed duplicate
4. Handle overlapping scans (same machine, nested paths)

### Folder Coverage Analysis

For each folder, calculate:
```
coverage = files_with_copies_elsewhere / total_files
```

Flags as "nearly redundant" if coverage > threshold (default 90%).

### Pre-flight Validation

Before any operation:
1. Source paths exist and are readable
2. Destination is writable (test write)
3. Sufficient disk space available

Validation happens **before** generating expensive artifacts (scripts, etc).

## Recovery & Checkpoints

### Scan Checkpoints

```go
type ScanCheckpoint struct {
    ManifestPath  string    // Which manifest
    ScanPath      string    // Which scan root
    ProcessedDir  int       // How many directories
    ProcessedFile int       // How many files
    LastFile      string    // Last file processed
    StartTime     time.Time // When scan started
    LastUpdate    time.Time // When progress was saved
}
```

Stored in: `~/manifests/_checkpoints/<manifest>.checkpoint` by default.
Tests and custom environments can override this with `PHOTO_ORGANIZER_CHECKPOINT_DIR`.

Interrupted scans are reported at startup so the source can be scanned again.

## Extending the Tool

### Adding a New Search Filter

In `runSearchAnalyze()`:
1. Parse flag: add to switch statement
2. Add filter function: e.g., `filterByCustomCriteria()`
3. Apply in loop: `if !filterByCustomCriteria(row) { continue }`
4. Add tests in `analyze_test.go`

Example:
```go
case arg == "-custom" && i+1 < len(args):
    customValue = args[i+1]
    i++
```

### Adding a New Subcommand

In `main.go`:
1. Add case to switch: `case "newcmd": runNewCommand(os.Args[2:]); return`
2. Create `runNewCommand()` in appropriate file
3. Update `printUsage()`
4. Add tests

### Custom Hash Algorithm

Edit `processFile()` in main.go:
- Change `sampleSize` constant
- Change hashing logic (currently uses md5)
- Update `sampleSize` constant (impacts performance/accuracy trade)

## Testing

### Run all tests
```bash
go test ./...
```

### Run specific test
```bash
go test -run TestBuildDeletePlanBasic ./...
```

### Run with verbose output
```bash
go test -v ./...
```

### Generate coverage report
```bash
go test -cover ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Test large datasets
Tests include:
- `TestBuildHashIndexLargeDataset`: 10,000+ files
- `TestFindDuplicatesLargeDataset`: cross-machine duplicates
- `TestScanDirectorySpecialChars`: filename edge cases

## Performance Characteristics

| Operation | Time | Input Size | Notes |
|-----------|------|-----------|-------|
| Scan | 10K files / 5s | Sampled hashing | Depends on disk I/O |
| Full hash | 1 file / 100ms | 1GB file | Used only on collisions |
| Build index | 1M hashes / 1s | All manifests | O(N) pass |
| Find duplicates | 1M files / 1s | All manifests | O(N) lookup |
| Duplicate report | 1M files / 5s | Includes display | O(N log N) |
| Search | Instant | All manifests | Grep-like, not indexed |
| Folder duplicate report | 10K files / 100ms | Folder signatures | Linear grouping |

## Manifest Versioning

Current format version: **1**

Fields are **additive**—older tools can read new manifests (ignoring extra columns).

To update manifest format:
1. Increment version in comments
2. Document new columns
3. Add migration function in `readManifest()`
4. Update tests

Example old format (still supported):
```
filename | relative_path | file_size_bytes | file_hash | extension
```

New format adds: `scan_date`, `scan_path`, `machine_name`, `partial_hash`, `full_hash`.

## Known Limitations

1. **No indexing**: Linear search, O(N) operations
2. **Memory-based**: Entire manifest loaded into RAM
3. **CSV format**: No transaction support, manual sync needed
4. **No encryption**: Manifests in plaintext
5. **Local caching**: Hash cache is per-manifest

## Future Optimization Ideas

1. Build SQLite index on top of CSVs (without changing format)
2. Stream-based analysis for very large manifests
3. Parallel scanning with worker pools
4. Background hash verification
5. Differential sync between machines
