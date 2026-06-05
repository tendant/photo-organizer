# Troubleshooting Guide

## Common Issues & Solutions

### SSH Connection Issues

#### "Connection refused"
```
⚠  Error: Connection refused (host not reachable or SSH not running)
   Try: ping nas.local  or  ssh -v nas.local
```

**Solutions**:
- Verify host is reachable: `ping nas.local`
- Check SSH is running: `ssh -v nas.local echo ok`
- Verify machine is in config: `photo-organizer collect --list`
- Add missing machine: `photo-organizer collect --add nas=user@nas.local`

#### "Permission denied"
```
⚠  Error: Permission denied (authentication failed)
   Try: ssh-keygen -t ed25519  or  ssh-copy-id nas.local
```

**Solutions**:
- Generate SSH key (if missing): `ssh-keygen -t ed25519`
- Add key to remote: `ssh-copy-id user@nas.local`
- Verify key works: `ssh user@nas.local echo ok`

#### "Connection timeout"
```
⚠  SSH verification timeout for nas.local (exceeded 30s)
   Tip: Set PHOTO_ORGANIZER_SSH_TIMEOUT=60s for slower networks
```

**Solutions**:
- Increase timeout: `export PHOTO_ORGANIZER_SSH_TIMEOUT=60s`
- Check network: `ping -c 10 nas.local` (check for packet loss)
- Use explicit timeout in plan: `photo-organizer plan --ssh-timeout 90s`

---

### Disk Space Issues

#### "Cannot write to destination"
```
⚠  Disk space for destination: cannot write to destination (need 500GB)
   → Check disk space and permissions
```

**Solutions**:
- Check available space: `df -h /path/to/destination`
- Free up space: `rm -rf /path/to/old/files`
- Use different destination with more space
- Split migration: migrate in multiple batches

#### "Low disk space warning"
```
⚠  Warning: Low disk space on /photos
   Cannot write 10MB test file
```

**Solutions**:
- Free up at least 100GB before large operations
- Check what's using space: `du -sh /photos/*`
- Archive old data before cleanup/migration

---

### Manifest Issues

#### "Manifest is 520 days old"
```
⚠  laptop @ /Photos: manifest is 520 days old (last scanned 2025-01-01)
```

**Meaning**: Manifest hasn't been updated in over 30 days—files may have changed.

**Solution**: Rescan to update:
```bash
photo-organizer rescan
```

#### "Skipped invalid rows"
```
⚠  test.csv: skipped 3 invalid row(s)
   row 3: empty relative_path
   row 4: invalid size "-1000"
```

**Meaning**: Some rows in the manifest are corrupted.

**Solution**: 
- Data is still usable with valid rows only
- Try regenerating manifest if many rows are corrupted:
```bash
rm /path/to/manifest.csv
photo-organizer scan /path/to/photos
```

#### "Missing required column"
```
analyze: no valid manifests loaded
```

**Meaning**: Manifest format is invalid or corrupted.

**Solutions**:
- Check manifest file exists: `ls -la ~/manifests/_Manifest/*.csv`
- Try different manifest: `photo-organizer analyze other.csv`
- Regenerate if broken: `photo-organizer scan /photos`

---

### Search Issues

#### "No matching files found"
```
No matching files found.
```

**Solutions**:
- Check filters are correct: `-name 'IMG_*'` (case-sensitive)
- Verify manifests loaded: `photo-organizer machines`
- Try broader search: `photo-organizer search -name '*'`
- Check file actually exists in manifests

#### "Wrong copy count showing"
```
Machine  Size    Hash      Copies  Full Path
test-1   1.9 MB  abc123de  2
```

If copies seem wrong:
- Ensure full_hash is populated (not just partial_hash)
- Hash falls back to partial_hash if full not available
- Check manifest has both machines: `photo-organizer machines`

---

### Migration Issues

#### "rsync failed"
```
⚠  rsync failed for group 1 (/data/Photos)
```

**Solutions**:
- Check source path exists: `ssh source-machine ls -la /data/Photos`
- Verify destination writable: `ssh dest-machine ls -la /destination`
- Check available space on destination: `ssh dest-machine df -h /destination`
- Try running migration script again (it resumes):
```bash
bash migration_script.sh
```

#### "Migration interrupted—how to resume?"
```
Startup message shows:
⚠  Found 1 incomplete scan(s). Run rescan to resume...
```

**Solution**: Just run the migration script again:
```bash
bash migration_script.sh
```

The script skips completed groups automatically via `.done` markers in `~/manifests/_migrate/`.

To **force full re-run**:
```bash
rm ~/manifests/_migrate/*/group_*.done
bash migration_script.sh
```

---

### Plan/Cleanup Issues

#### "No files unique to machine"
```
migrate: no files unique to test-machine
```

**Meaning**: All files on test-machine exist elsewhere (no migrations needed).

**Solution**: 
- This is actually good—everything is backed up!
- Check duplicates instead: `photo-organizer analyze`

#### "No duplicates found"
```
plan: no duplicates found between machines
```

**Solution**:
- Each machine has unique files
- No cleanup needed (everything is unique)
- Consider running `risk-report` to see what's at risk

#### "Keep machine not found"
```
plan: machine "laptop" not found in manifests
Available machines: phone camera
```

**Solution**:
- Use correct machine name: `photo-organizer plan --keep phone`
- List available machines: `photo-organizer machines`

---

### Performance Issues

#### "Scanning is slow"
Scanning 100K files takes >1 minute:

**Solutions**:
- Use `--auto-identify-folders` to scan only photo folders
- Run on local machine (not over SSH)
- Check disk I/O: `iostat 1` while scanning
- For very large scans, split into smaller paths

#### "Analysis is slow"
Analyzing 1M files takes >10 seconds:

**Solution**: This is normal—CSV analysis is O(N log N)
- If unacceptable, consider maintaining multiple smaller manifests
- Split analysis: `photo-organizer analyze subset1.csv subset2.csv`

#### "Search timeout on huge dataset"
Search hangs on 10M+ files:

**Solutions**:
- Use more specific filters: `-name` pattern, `-size`, `-date`
- Search only relevant manifests: `photo-organizer search laptop.csv`
- Export and grep CSV: `grep 'IMG_' ~/manifests/_Manifest/laptop.csv`

---

### Recovery & Rollback

#### "Accidentally uncommented rm lines—how to undo?"
If you already ran the script:

**Solution**: Files moved to quarantine, not deleted:
```bash
# Move back from quarantine
mv /Photos/_quarantine/photo-organizer/* /Photos/
```

#### "Lost manifest file"
If you accidentally deleted a CSV manifest:

**Solution**: You can rescan:
```bash
# On the original machine
photo-organizer scan /path/to/photos

# Then collect manifests again
photo-organizer collect
```

---

## Getting Help

### Enable verbose logging
```bash
export PHOTO_ORGANIZER_DEBUG=1
photo-organizer analyze
```

### Check manifests config
```bash
cat ~/manifests/machines.conf
ls -la ~/manifests/_Manifest/
```

### Verify pre-flight checks
```bash
photo-organizer scan /test/path 2>&1 | head -10
# Shows: ✓ Source directory readable, ✓ Manifest directory writable
```

### Test SSH connection
```bash
photo-organizer collect --list
photo-organizer plan --keep machine1 --ssh nas.local test.csv
```

---

## When to Contact Support

Include:
1. Output from failing command (full error message)
2. Machine and OS info: `uname -a`
3. Photo Organizer version: `photo-organizer --version` (if available)
4. Manifest info: `wc -l ~/manifests/_Manifest/*.csv`
5. Disk space: `df -h ~/manifests`

Example:
```bash
uname -a > ~/error-report.txt
photo-organizer analyze >> ~/error-report.txt 2>&1
cat ~/error-report.txt
```
