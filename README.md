# Photo Organizer

**Scan multiple machines, find duplicates across your photo library, and safely verify backups.**

A command-line tool that discovers duplicate photos across machines, checks backup coverage, and copies missing files to archives.

## Quick Start

### Scan your photos
```bash
photo-organizer scan ~/Photos
```
Creates a manifest CSV in `~/manifests/_Manifest/`.

### Find duplicates across machines
```bash
photo-organizer dups ~/manifests/_Manifest/*.csv
```
Shows:
- Which machines have which duplicates
- Files at risk (only on one machine)
- Folder redundancy analysis

### Find duplicate folders for cleanup
```bash
photo-organizer dup-folders --top 20 ~/manifests/_Manifest/*.csv
```
Shows folders with overlapping file sets so you can clean up whole folders safely.

### Back up files missing from an archive
```bash
photo-organizer backup-missing ~/Photos --dest nas:/backups/photos
```
Copies only files that are not already represented in collected manifests.

### Search across all machines
```bash
photo-organizer search -name 'IMG_*' -duplicates-only
photo-organizer search -size '100MB-500MB' -group
photo-organizer search -hash abc123def456
```

## Key Features

✅ **Safe**: All deletions require manual review (scripts are commented out)  
✅ **Distributed**: CSV manifests live on each machine, easy to sync  
✅ **Fast**: Sampled hashing (first + last 32KB) finds duplicates in seconds  
✅ **Reliable**: Handles corrupted manifests, SSH timeouts, interrupted scans  
✅ **Resumable**: Migrations can pause and resume without re-transferring  
✅ **Smart**: Pre-flight validation prevents wasted time on broken operations  

## Commands

| Command | Purpose |
|---------|---------|
| `scan [dir]` | Scan directory and create manifest |
| `dups` | Find duplicate files across manifests |
| `dup-folders` | Find duplicate folders for cleanup |
| `storage-status` | Show storage breakdown by machine and device |
| `storage-plan` | Recommend what to back up next |
| `backup` | Back up a folder to a timestamped archive |
| `backup-missing` | Copy only files not already backed up |
| `search` | Query manifests by name/hash/size/date |
| `collect` | Pull manifests from remote machines |
| `machines` | List all machines in manifests |
| `check-backup` | Check if a folder is backed up |

## Configuration

**Machines config**: `~/manifests/machines.conf`
```
laptop=lei@laptop.local
nas=lei@nas.local
```

Add machines with:
```bash
photo-organizer collect --add nas=user@host
```

## Workflow Examples

See [USAGE.md](USAGE.md) for:
- Organizing photos by date across machines
- Safe deduplication strategies
- Backup verification
- Large-scale migrations
- Handling interrupted operations

## Troubleshooting

See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for solutions to:
- SSH connection issues
- Disk space problems
- Manifest corruption
- Timeout handling
- Resume interrupted scans

## Performance

- **Scanning**: 10,000 files in ~5 seconds (sampled hashing)
- **Analysis**: 1M files across 5 machines in <1 second
- **Search**: Instant across any number of manifests

## Safety Guarantees

1. **No automatic deletion** — All `rm` commands are commented out
2. **No data loss** — Pre-flight checks prevent broken operations
3. **Resume on interrupt** — Transfers survive network failures
4. **Easy rollback** — All changes can be undone
5. **Verification** — SSH backup validation before cleanup

## For Developers

See [API.md](API.md) for:
- Internal data structures
- Hash algorithm details
- Manifest format specification
- Custom tool development

## License

MIT
