# Photo Organizer – Current Status & Roadmap

**Last Updated:** 2026-06-06  
**Project Stage:** Phase 4 complete (Machines config, edge case robustness, documentation)

---

## ✅ Completed Features

### Core Infrastructure
- [x] CLI structure with subcommands (scan, analyze, plan, migrate, rescan, collect)
- [x] Manifest CSV format with versioning and backward compatibility
- [x] Machine ID generation and stability (`~/.photo-organizer-id`)
- [x] Manifest backup system (keeps last 5 backups in `_backups/`)

### Scanning & Hashing
- [x] Recursive directory scanning with progress indication
- [x] Metadata extraction (filename parsing, EXIF dates)
- [x] Sampled partial hash (first 32KB + last 32KB of files)
- [x] Full-file MD5 hash for confirmed duplicates
- [x] Smart hash upgrade (files with colliding partial hashes get full hash)
- [x] Caching system to skip re-hashing unchanged files
- [x] `--full-hash` flag to force full hashing
- [x] `--no-cache` flag to recompute all hashes
- [x] File type filtering (photos, videos, audio, sidecars)
- [x] System folder skipping (PRIVATE, THMBNL, AVF_INFO, etc.)
- [x] Symlink detection with warnings

### Rescanning
- [x] `rescan` command to update existing manifests
- [x] `--prune` flag with safety guard (skips if >50% would be removed)
- [x] Per-machine rescan based on previous scan paths
- [x] `--machine` flag to target specific machine
- [x] `--root` flag to specify manifest directory

### Analysis
- [x] Manifest merging across multiple machines
- [x] Duplicate group detection (confirmed vs unconfirmed)
- [x] Unique file identification (single-machine files)
- [x] Intra-machine duplicate detection
- [x] Folder redundancy analysis with coverage percentages
- [x] Stale manifest warnings (30+ days without rescan)
- [x] Per-machine file count and size breakdown
- [x] CSV output export

### Planning & Scripts
- [x] Cross-machine cleanup planning (`plan --keep <machine>`)
- [x] Intra-machine deduplication (`plan --intra <machine>`)
- [x] Safe delete script generation (all `rm` commands commented out)
- [x] `--keep-under <path>` for intra-machine dedupe strategy
- [x] SSH backup verification (`plan --ssh user@host`)
- [x] Backup path flagging (verified vs unverified)

### Migration
- [x] Unique file migration planning (`migrate --from <machine>`)
- [x] Rsync script generation with `--partial` and `--progress` flags
- [x] Folder structure preservation
- [x] Remote destination support (user@host:/path)
- [x] File list management via `~/manifests/_migrate/`
- [x] Resume via `.done` markers (skip completed groups)
- [x] Pre-flight validation (source paths, destination writable, disk space)
- [x] Cleanup on script exit via trap
- [x] Re-run safety (rsync skips completed files)

### Configuration & Metadata
- [x] Machines config file for machine metadata
- [x] `collect` subcommand to gather manifests
- [x] `search` subcommand with comprehensive filtering
  - [x] Search by filename pattern (glob/regex)
  - [x] Search by path substring
  - [x] Search by hash (find all copies)
  - [x] Search by size (exact or range)
  - [x] Search by date range
  - [x] Filter by machine
  - [x] Duplicates-only mode
  - [x] Group by hash display
  - [x] CSV export

### Edge Case Robustness (Phase 4)
- [x] Checkpoint system for resuming interrupted scans
- [x] SSH timeout handling with `PHOTO_ORGANIZER_SSH_TIMEOUT` config
- [x] Comprehensive SSH error messages and help text
- [x] Pre-flight validation (source readable, dest writable, disk space available)
- [x] Disk space monitoring during scans and migrations
- [x] Manifest validation with graceful handling of corrupted rows
- [x] Migration resume via `.done` markers (skip completed groups)
- [x] Comprehensive test suite (65 test cases)

### Documentation (Phase 4)
- [x] README.md — Quick start, features, performance metrics
- [x] USAGE.md — 10 real-world workflow examples
- [x] TROUBLESHOOTING.md — Solutions for common issues and recovery procedures
- [x] API.md — Data structures, CSV format, algorithms, extension guidelines
- [x] REMOVABLE_MEDIA.md — SD card workflows, multi-camera handling
- [x] BACKUP_SD_CARD.md — Complete SD card backup workflow guide

---

## 🔄 In Progress / Known Gaps

### Future Enhancements
- [ ] Video walkthrough for common workflows
- [ ] Performance benchmarking on real datasets (100K+ files)
- [ ] Integration tests for SSH-based backups
- [ ] Face recognition and smart organization
- [ ] Web UI dashboard for duplicate exploration

---

## 📋 Potential Future Features (Priority Order)

### High Priority
1. **SQLite backend** — Replace CSV for faster queries on large manifests
   - Index by full_hash for O(1) lookups
   - Index by machine_id, scan_date for freshness queries
   - Atomic transactions for consistency

2. **Perceptual hashing** — Detect near-duplicate photos
   - Use image fingerprinting (dhash, phash)
   - Detect re-encoded videos, rotated images
   - Useful for finding subtle duplicates missed by full hash

3. **Web UI** — Browser-based visualization
   - Duplicate group explorer
   - Machine inventory dashboard
   - Risk visualization (unique-at-risk files)
   - Plan review interface before running scripts

### Medium Priority
4. **Parallel scanning workers** — Distribute scanning across machines via SSH
5. **Face recognition** — Group photos by people
6. **Smart organization** — Organize by date, event, camera, location (EXIF)
7. **Incremental cleanup** — Interactive duplicate selection instead of scripts
8. **Compression analysis** — Estimate space savings from cleanup

### Low Priority (Nice-to-Have)
9. **Cloud storage integration** — Scan Google Photos, Dropbox, iCloud
10. **Distributed backend** — PostgreSQL for team photo libraries
11. **AI tagging** — Auto-generate labels from image content
12. **Deduplication verification** — Cryptographic proof of deletion

---

## 🐛 Known Issues & Workarounds

### Current Limitations
- **CSV scaling** — Slow on manifests >1M files (use SQLite for production)
- **SSH over slow connections** — `plan --ssh` may timeout on high-latency links
- **No automatic cleanup** — Requires manual script review (by design, for safety)
- **Windows symlinks** — May behave unexpectedly on Windows
- **iCloud photos** — Can't scan iCloud Photos Library directly (need manual export)

### Workarounds
- For very large manifests: Consider splitting into per-volume scans
- For SSH timeouts: Use `--timeout` flag (needs implementation)
- For Windows: Use WSL2 for consistent symlink handling

---

## 🚀 Recommended Next Steps (Short-term)

### Phase 5: Web UI (Future Enhancement)
- Simple React dashboard for duplicate exploration
- Duplicate group browser with preview
- Script review interface before execution
- **Effort:** 4-7 days
- **Payoff:** Much safer user experience for large libraries

### Phase 5: Advanced Features (Optional)
- **SQLite backend** — For manifests >1M files (user chose CSV for now)
- **Perceptual hashing** — Detect near-duplicate photos
- **Face recognition** — Group photos by people
- **Smart organization** — Auto-organize by date, event, camera, location

---

## 📊 Architecture Notes

### Current Design Decisions
- **Append-only manifests:** Preserves history, enables rollback
- **CSV format:** Human-readable, easy to debug, versioned for compatibility
- **Sampled hashing:** 99.9% accuracy with 1000x faster scanning than full hash
- **Commented-out deletes:** Forces review, prevents accidental data loss
- **SSH verification:** Ensures backup files actually exist before cleanup

### Scalability Limits (Current CSV-based)
- 1M files: ~1 second load time
- 10M files: ~10 seconds load time
- 100M files: Not practical (need SQLite or database)

### Suggested Optimizations
1. Index manifests by full_hash for O(1) duplicate detection
2. Cache duplicate groups between runs
3. Parallelize file list generation in migrate
4. Stream manifest reads instead of loading into memory

---

## 🎯 Success Criteria

The tool is successful when:
- ✅ Users can discover 90%+ of duplicate files across multiple machines
- ✅ No accidental data loss (all deletes require explicit review)
- ✅ Manifests remain accurate after months of rescans
- ✅ Handles photo libraries 100K+ files without manual intervention
- ✅ SSH-based backup verification works reliably
- ✅ Migration scripts can resume after interruption

---

## 🔐 Safety Guarantees (Maintained)

1. **Never delete without backup verification**
   - All cleanup scripts have `rm` commands commented out
   - `plan --ssh` verifies files exist before suggesting deletion
   - Users must manually uncomment each `rm` before running

2. **Manifest integrity**
   - Automatic backups before every write (last 5 retained)
   - CSV versioning for backward compatibility
   - Append-only design preserves history

3. **Distributed safety**
   - Works with disconnected drives (manifest caching)
   - Handles machines offline for weeks
   - No single point of failure

