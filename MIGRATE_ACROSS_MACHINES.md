# Running Migration Scripts Across Machines

The migration script is **self-contained and can run from any machine**, as long as it can access the source files (locally or via network/SSH).

## Scenarios

### Scenario 1: Run on the Same Machine (Default)

**Machine A** has the camera SD card. You generate and run the script on Machine A.

```bash
# On Machine A (has source files)
photo-organizer migrate --from camera --dest /network/backup --out migrate.sh
bash migrate.sh
```

### Scenario 2: Generate on One Machine, Run on Another

**Machine A** (with manifests) generates the script. **Machine B** runs it to copy from Machine A.

**Step 1: Generate script on Machine A**
```bash
# Machine A: has the photo-organizer manifests
photo-organizer migrate --from camera --dest user@machineB:/backup --out migrate.sh
# Gives you: migrate.sh
```

**Step 2: Copy script to Machine B**
```bash
scp migrate.sh user@machineB:~/
```

**Step 3: Run on Machine B**
```bash
# Machine B: must be able to access the source paths from Machine A
ssh user@machineB
bash ~/migrate.sh
```

**Important:** The script uses absolute paths from Machine A. If running from Machine B, you need:
- **Network access** to the source files (NFS mount, Samba share, SSHFS)
- **Same absolute paths**, OR modify the script paths manually

### Scenario 3: Remote Source, Local Destination

Copy files from a remote machine's mounted network share to local backup.

**Prerequisites:**
- Source machine mounted at `/mnt/remote-camera` (via NFS, Samba, or SSHFS)
- Destination is local `/backup`

**Generate script:**
```bash
# Uses /mnt/remote-camera as source
photo-organizer migrate --from camera --dest /backup --out migrate.sh
bash migrate.sh
```

The script automatically uses `rsync` which handles remote paths well.

### Scenario 4: Multiple Destination Machines

Generate one script that syncs to multiple backups.

**Generate separate scripts for each destination:**
```bash
photo-organizer migrate --from camera --dest /local/backup --out migrate_local.sh
photo-organizer migrate --from camera --dest user@nas:/backup --out migrate_nas.sh
photo-organizer migrate --from camera --dest user@cloud:/backup --out migrate_cloud.sh
```

**Run them sequentially:**
```bash
bash migrate_local.sh      # Copy to local disk
bash migrate_nas.sh        # Copy to NAS
bash migrate_cloud.sh      # Copy to cloud
```

Each script tracks progress independently via `~/.manifests/_migrate/`

## Understanding What Paths the Script Uses

When you generate a script with:
```bash
photo-organizer migrate --from camera --dest /backup
```

The script contains:
1. **Source paths** — Absolute paths from the camera's scan
2. **Destination path** — Where you're copying to (`/backup`)
3. **Temp directory** — Always `~/.manifests/_migrate/` (creates locally)

### Example with Absolute Paths

Generated script includes:
```bash
WORK_DIR="/Users/lei/manifests/_migrate"

# Source is absolute path from manifest
rsync -av --checksum --partial --progress \
        --files-from=$WORK_DIR/migrate_1.txt \
        '/Volumes/Untitled/' \              # ← Absolute source
        '/Users/lei/backup/Untitled/'       # ← Absolute destination
```

**When running from a different machine:**
- If you have `/Volumes/Untitled` mounted via NFS or SSHFS, it works as-is
- If the path is different, you need to edit the script:

```bash
# Before running, substitute source paths:
sed -i.bak "s|/Volumes/Untitled|/mnt/camera|g" migrate.sh
```

## Recommended Workflow for Cross-Machine Migrations

### Step 1: Configure Network Access

Mount the source machine's files on your backup machine:

**Option A: NFS Mount** (Linux/Mac)
```bash
# On backup machine
mkdir -p /mnt/camera
mount -t nfs remote-machine:/path/to/files /mnt/camera
```

**Option B: SSHFS Mount** (Any OS)
```bash
# On backup machine
mkdir -p /mnt/camera
sshfs user@remote-machine:/Volumes/Untitled /mnt/camera
```

**Option C: Samba/SMB Mount** (Windows/Mac/Linux)
```bash
# On backup machine  
mkdir -p /mnt/camera
mount -t cifs //remote-machine/share /mnt/camera
```

### Step 2: Generate Script (Targeting Mounted Path)

```bash
# Option 1: Generate on source machine (easier)
# On Machine A (has source files):
photo-organizer migrate --from camera --dest /local/backup --out migrate.sh

# Option 2: Generate on backup machine (requires network access)
# On Machine B, after mounting:
photo-organizer migrate --from camera --dest /backup --out migrate.sh
# Edit paths if needed
```

### Step 3: Run Migration

```bash
# On Machine B (backup machine)
bash migrate.sh

# Output shows:
# [2026-06-06 14:30:45] [1/5] Copying 2,500 files (145.1 GB) to: /backup/camera
#   sending incremental file list
#   DCIM/123LEICA/L1230001.DNG
#   DCIM/123LEICA/L1230001.JPG
#   ...
# [✓] Group 1 complete
# [✓] Group 2 complete
# ...
# ✓ MIGRATION COMPLETE: All 12,331 files transferred
# You can now safely delete from source or reformat media.
```

### Step 4: Verify and Resume

**Resume if interrupted:**
```bash
bash migrate.sh  # Automatically skips already-copied files
```

**Verify completion:**
```bash
# Check that progress files exist
ls ~/.manifests/_migrate/migrate_*.done

# All groups should have .done files if complete
```

## Troubleshooting Cross-Machine Migrations

### "Path not found" or "rsync: command not found"

**Causes:**
- `rsync` not installed on destination machine
- Source path not mounted or not accessible

**Fix:**
```bash
# Ensure rsync is installed
which rsync
# If not found:
apt install rsync        # Ubuntu/Debian
brew install rsync       # macOS
```

### "Permission denied" errors

**Causes:**
- User doesn't have read permission on source
- User doesn't have write permission on destination

**Fix:**
```bash
# Check source permissions
ls -la /mnt/camera

# Check destination permissions
ls -la /backup

# If needed, change ownership:
sudo chown $(whoami) /backup
sudo chmod 755 /mnt/camera
```

### "No space left on device"

**Cause:**
- Destination doesn't have enough free space

**Fix:**
```bash
# Check available space
df -h /backup

# Resume later after freeing space
bash migrate.sh
```

### Different Path on Each Machine

**Scenario:**
- Machine A: source is at `/Volumes/SSD/photos`
- Machine B: mounted at `/mnt/network/photos`

**Solution:**
```bash
# Edit the script before running
sed -i.bak 's|/Volumes/SSD|/mnt/network|g' migrate.sh

# Verify changes
head -30 migrate.sh

# Then run
bash migrate.sh
```

## Advanced: SSH-Based Remote Sync

If you don't want to mount filesystems, use SSH-based rsync directly:

**Generate script for remote source:**
```bash
# Generate normally first
photo-organizer migrate --from camera --dest /local/backup --out migrate.sh
```

**Edit script to use SSH:**
```bash
# Before rsync, add SSH command in the script:
# rsync -av --checksum --partial --progress \
#        -e "ssh -i ~/.ssh/id_rsa" \
#        --files-from=$WORK_DIR/migrate_1.txt \
#        'user@remote-machine:/Volumes/SSD/' \
#        '/local/backup/'
```

Or use rsync URL syntax:
```bash
rsync -av rsync://user@remote-machine/path file_list /backup/
```

## Best Practices

✅ **Always verify network connectivity first**
```bash
ping remote-machine
ssh user@remote-machine ls /path/to/source
```

✅ **Test with small subset first**
```bash
# Modify script to copy just one folder
sed -i 's|DCIM/.*|DCIM/101LEICA|' migrate.sh
bash migrate.sh

# Verify it worked
ls /backup/camera/DCIM/101LEICA/
```

✅ **Keep manifests synchronized**
```bash
# Before generating script, ensure manifests are up-to-date
photo-organizer collect --from remote-machine
photo-organizer migrate --from camera --dest /backup
```

✅ **Monitor progress in separate terminal**
```bash
# Terminal 1: Run migration
bash migrate.sh

# Terminal 2: Monitor progress
watch -n 2 'du -sh /backup && ls ~/.manifests/_migrate/*.done | wc -l'
```

✅ **Use `--no-report` if running from cron**
```bash
# For automated backups, skip the interactive report
photo-organizer scan /source --no-report
photo-organizer migrate --from backup --dest /destination --out migrate.sh
bash migrate.sh >> /var/log/backup.log 2>&1
```

## Quick Reference

| Scenario | Command |
|----------|---------|
| Same machine | `photo-organizer migrate --from camera --dest /backup` |
| Different machine, mounted source | `photo-organizer migrate --from camera --dest /backup` |
| Remote NAS destination | `photo-organizer migrate --from camera --dest user@nas:/backup` |
| Multiple destinations | Generate separate scripts with different `--dest` |
| Test first | Edit script to limit to one folder, run, verify |
| Resume interrupted | `bash migrate.sh` (re-run same script) |

---

**Key insight:** The migration script is just rsync in a self-contained shell script. It works anywhere rsync can reach the source files, whether local, networked, or remote.
