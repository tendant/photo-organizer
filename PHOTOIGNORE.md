# .photoignore: Exclude Files from Backup

Create a `.photoignore` file in any folder to exclude files from backup scanning. Works like `.gitignore`.

## Quick Start

```bash
cat > ~/Photos/.photoignore <<'EOF'
.claude/
_Manifest/
*.tmp
Thumbs.db
EOF
```

Then refresh the scan:
```bash
photo-organizer scan ~/Photos
```

## Pattern Syntax

- `*.jpg` — Match all `.jpg` files
- `dirname/` — Skip entire directory (note trailing `/`)
- `*.tmp` — Match files by extension
- `[abc]*` — Match files starting with a, b, or c
- `#` — Comments and blank lines are ignored

## How It Works

1. **Automatic discovery** — `.photoignore` files cascade from root to subfolders
   - Root `.photoignore` patterns apply everywhere
   - Subfolder patterns only apply to that subfolder and below

2. **Applies to all commands** — `scan`, `analyze`, and `verify` all respect patterns

3. **Not retroactive** — Only affects new scans. Use `prune` to clean up old entries

## Example

```
~/Photos/
  .photoignore              # *.tmp blocks all .tmp files everywhere
    ├── 2024/
    │   .photoignore        # draft/ only blocks in 2024/ and subfolders
    │   draft/
    │   photo.jpg           ✓ included
    │   temp.tmp            ✗ excluded (matches root pattern)
    └── exports/
        *.psd               ✓ included (psd pattern only in exports/)
```

## Common Exclusions

**Photo backup:**
```
_Manifest/
.claude/
*.tmp
*.cache
```

**Project folder:**
```
.vscode/
.idea/
node_modules/
dist/
build/
*.swp
```

## Hardcoded Skips

Only OS metadata is always skipped (can't be changed):
- `.DS_Store` (macOS)
- `.stfolder/` (Syncthing)

Everything else is included by default unless you add `.photoignore` patterns.

## Tips

- **Verify what was scanned:** `photo-organizer search -path Photos`
- **Clean up old entries:** After adding patterns to `.photoignore`, run `photo-organizer scan ~/Photos --prune`
- **Multiple .photoignore files:** Each folder can have its own patterns
