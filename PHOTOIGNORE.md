# .photoignore: Control File Exclusions from Backup

`photo-organizer` supports `.photoignore` files to control which files and folders are excluded from backup scanning. This works similarly to `.gitignore` in Git repositories.

## Overview

By default, `photo-organizer` only excludes essential OS and sync metadata:
- `.DS_Store` (macOS system file)
- `.stfolder/` (Syncthing sync metadata)

Everything else is backed up unless you create a `.photoignore` file to exclude it.

## Creating a .photoignore File

Create a text file named `.photoignore` in the folder you want to scan (or any subfolder):

```bash
cat > ~/Photos/.photoignore <<'EOF'
.claude/
_Manifest/
*.tmp
Thumbs.db
EOF
```

## Pattern Syntax

### Basic Patterns

- `*.jpg` — Matches all `.jpg` files
- `cache*` — Matches files starting with "cache"
- `temp-?.txt` — Matches `temp-1.txt`, `temp-2.txt`, etc.
- `[abc]*` — Matches files starting with a, b, or c

### Directory Patterns

- `dirname/` — Skips entire directory named "dirname" (include trailing `/`)
- `.claude/` — Skips `.claude` folder and everything inside
- `node_modules/` — Skips `node_modules` folder

### Comments and Blank Lines

```
# This is a comment
.claude/              # Skip Claude metadata

# Blank lines are ignored
_Manifest/

*.tmp                 # Skip temporary files
```

## Cascade Behavior

`.photoignore` files cascade from the scan root down to subfolders:

```
~/Photos/
  .photoignore          ← Root patterns apply everywhere
  └── 2024/
      .photoignore      ← These patterns only apply to 2024/ and subfolders
      └── January/
          photo.jpg
```

Example:

**Root `.photoignore`:**
```
*.tmp
```

**Subdir `.photoignore` (`2024/.photoignore`):**
```
drafts/
```

Result:
- All `.tmp` files skipped everywhere (from root pattern)
- `2024/drafts/` folder skipped (from subdir pattern)
- `drafts/` in other locations NOT skipped (subdir pattern doesn't cascade up)

## Common Examples

### Typical Project Structure

```bash
cat > ~/Projects/MyProject/.photoignore <<'EOF'
# Development artifacts
.claude/
.vscode/
.idea/
node_modules/
dist/
build/

# Temporary files
*.tmp
*.swp
*.bak

# IDE metadata
Thumbs.db
.DS_Store

# Editor backups
*~
*.swp
EOF
```

### Photo Backup

```bash
cat > ~/Photos/.photoignore <<'EOF'
# System metadata
_Manifest/
.claude/

# Temporary files
*.tmp
*.cache

# Editing software projects (not needed, only exports)
*.psd
*.xcf
*.afphoto
EOF
```

### Media Archive

```bash
cat > /mnt/archive/.photoignore <<'EOF'
# Exclude extraction folders
_extracted/
_temp/

# Exclude work-in-progress
WIP/
draft/

# Exclude cache files
*.cache
*.lock
EOF
```

## Tip: Hints During Scan

When scanning a folder without a `.photoignore` file, you'll see:

```
💡 Tip: Create a .photoignore file to exclude folders/files from backup
   Example: echo '.claude/
_Manifest/' > .photoignore
```

This helps you remember to set up exclusions.

## Integration with Scanning

### Before Backup

When you run `scan`, `.photoignore` patterns are automatically applied:

```bash
photo-organizer scan ~/Photos
```

This will:
1. Load `.photoignore` patterns from `~/Photos/` and subfolders
2. Skip any files matching the patterns
3. Back up only included files

### Updating Exclusions

If you add or modify a `.photoignore` file:

```bash
# Create new .photoignore
echo "*.tmp" > ~/Photos/.photoignore

# Rescan to apply new exclusions
photo-organizer scan ~/Photos
```

The new scan will use updated exclusions. Old manifest entries for excluded files are not automatically cleaned up — use `prune` to clean them:

```bash
photo-organizer prune ~/Photos
```

## Testing Exclusions

To see what would be excluded without actually scanning:

```bash
# Create a test .photoignore
cat > ~/Photos/.photoignore <<EOF
test*.jpg
EOF

# Run analyze to preview what would be scanned
photo-organizer analyze ~/Photos
```

The `analyze` command also respects `.photoignore` patterns.

## Migration from Old Backups

If you had previously backed up files you want to exclude (e.g., `.claude/`), the old manifest entries remain. To clean them up:

1. Create `.photoignore`:
   ```bash
   echo ".claude/" > ~/Photos/.photoignore
   ```

2. Rescan:
   ```bash
   photo-organizer scan ~/Photos
   ```

3. Prune old entries:
   ```bash
   photo-organizer prune ~/Photos
   ```

This removes manifest entries for files that are now excluded.

## FAQ

**Q: Can I have multiple .photoignore files?**  
A: Yes! Place them in different folders. Patterns cascade from root down to subfolders.

**Q: Do .photoignore patterns affect verification?**  
A: Yes. The `verify` command also respects `.photoignore` patterns and only checks files that would be included in a scan.

**Q: What if I exclude a file that was already backed up?**  
A: The old manifest entry remains. Use `prune` to clean up old entries for excluded files.

**Q: Are .photoignore changes retroactive?**  
A: No. Only new scans apply the updated patterns. Existing manifest entries are not automatically removed.

**Q: Can I exclude by file size or date?**  
A: No. `.photoignore` only supports glob patterns for file/folder names. For more complex filtering, use the command-line flags or create manual folder structures.

**Q: What about .gitignore format compatibility?**  
A: `.photoignore` uses a simplified glob format, not full gitignore syntax. Features like negation (`!include`) and `**` recursive wildcards are not supported.
