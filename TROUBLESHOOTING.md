# Troubleshooting Guide

Common issues and current commands to diagnose them.

## SSH And Collection

### Connection Refused

```text
Error: Cannot connect to nas.local
```

Check that the host is reachable and SSH is running:

```bash
ping nas.local
ssh -v nas.local echo ok
photo-organizer collect --list
```

Register missing machines:

```bash
photo-organizer collect --add nas=user@nas.local
```

### Permission Denied

```text
Permission denied when connecting
```

Fix SSH authentication:

```bash
ssh-keygen -t ed25519
ssh-copy-id user@nas.local
ssh user@nas.local echo ok
```

### Slow Or Timing Out SSH

Increase the timeout for remote operations:

```bash
export PHOTO_ORGANIZER_SSH_TIMEOUT=120s
photo-organizer collect --from nas
```

## Scan And Manifest Issues

### Manifest Is Old

```text
manifest is 520 days old
```

Scan the source folder again:

```bash
photo-organizer scan ~/Photos --prune
```

### Invalid Rows Were Skipped

```text
skipped 3 invalid row(s)
```

The valid rows are still usable. If many rows are invalid, regenerate the manifest from the original source:

```bash
photo-organizer scan /path/to/photos --no-cache --prune
```

### Missing Required Column

```text
no valid manifests loaded
```

Check that the file is a photo-organizer manifest:

```bash
ls -la ~/manifests/_Manifest/*.csv
head -1 ~/manifests/_Manifest/example.csv
photo-organizer scan /path/to/photos
```

## Search And Duplicate Results

### No Matching Files

Try broader filters and confirm manifests exist:

```bash
photo-organizer manifests
photo-organizer machines
photo-organizer search -name '*'
```

### Duplicate Counts Look Wrong

Refresh manifests and check for overlapping scans:

```bash
photo-organizer scan ~/Photos --prune
photo-organizer dups
photo-organizer dup-folders --top 20
```

Nested scan roots can make the same physical file appear in multiple manifests. The analyzer attempts to account for this, but reviewing `manifests` output is still useful.

## Backup Issues

### Destination Cannot Be Written

```text
cannot write to destination
```

Check space and permissions:

```bash
df -h /path/to/destination
touch /path/to/destination/.write-test
rm /path/to/destination/.write-test
```

### rsync Failed During backup-missing

Check the configured destination and remote path:

```bash
photo-organizer collect --list
ssh user@host df -h /backups
ssh user@host ls -la /backups
```

Then retry:

```bash
photo-organizer backup-missing ~/Photos --dest user@host:/backups/photos
```

### check-backup Shows At-Risk Files

Back up the folder, collect manifests, then check again:

```bash
photo-organizer backup ~/Photos /mnt/archive
photo-organizer collect
photo-organizer check-backup ~/Photos
```

## Archive Integrity

### verify-archive Reports Missing Files

Verify the archive path exists, then repair only after reviewing the report:

```bash
photo-organizer verify-archive ~/manifests/_Manifest/photos.csv
photo-organizer fix ~/manifests/_Manifest/photos.csv /mnt/archive/2026-06-20-143022-Photos
```

### Accidental Cleanup

Prefer `archive` before deletion so folders are recoverable:

```bash
photo-organizer archive ~/Photos/OldImport
photo-organizer list ~/Photos/_archive
```

If files were moved into a quarantine folder by an older generated script, move them back manually from that quarantine path.

## Performance

### Scanning Is Slow

- Run scans on local disks when possible.
- Use `.photoignore` for generated or temporary files.
- Split very large libraries into stable top-level folders.

```bash
photo-organizer scan ~/Photos/2024
photo-organizer scan ~/Photos/2025
```

### Search Or Duplicate Analysis Is Slow

Use narrower inputs or filters:

```bash
photo-organizer dups ~/manifests/_Manifest/*laptop*.csv
photo-organizer search -name '*.mp4' -size '500MB-'
```

## Getting Help

Capture the command output and basic environment details:

```bash
uname -a > ~/photo-organizer-report.txt
photo-organizer manifests >> ~/photo-organizer-report.txt 2>&1
photo-organizer machines >> ~/photo-organizer-report.txt 2>&1
df -h ~/manifests >> ~/photo-organizer-report.txt 2>&1
```

Include the failing command, the full error message, and relevant manifest paths.

## Deprecated Command Names

If an older note suggests `analyze`, `plan`, `migrate`, `verify-backup`, `sign-manifest`, or `repair-manifest`, use the current commands: `dups`, `dup-folders`, `backup-missing`, `verify-archive`, `sign`, and `fix`.
