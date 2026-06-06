# Verifying Scan Coverage

The file type coverage report is **shown by default** before every scan. This ensures you always see what will be captured before the scan starts.

To see the report (default):
```bash
photo-organizer scan /Volumes/MyCamera
# Report shows automatically before scanning starts
```

To skip the report (for automated/scripted scans):
```bash
photo-organizer scan /Volumes/MyCamera --no-report
# Skips report and starts scanning immediately
```

## What You'll See

The report shows:

```
═════════════════════════════════════════════════════════
File Type Coverage Report: /Volumes/MyCamera
═════════════════════════════════════════════════════════

Files to be SCANNED:
  Type            Count         Size
  ────            ─────         ────
  .mp4               28     145.1 GB      ✓ Will be scanned
  .hif             1849      15.7 GB      ✓ Will be scanned
  .jpg               28       2.6 MB      ✓ Will be scanned
  TOTAL            1905     160.8 GB

Files to be IGNORED (not media):
  Type            Count         Size
  ────            ─────         ────
  .bin                1       9.2 MB      ✗ Will be skipped
  .xml               29      72.7 KB      ✗ Will be skipped
  .txt                2       4.0 KB      ✗ Will be skipped
  TOTAL              41      13.4 MB
```

## Understanding the Report

### Files to be SCANNED
These file types are recognized as photos, videos, audio, or sidecars:
- **Photos**: .jpg, .raw, .nef, .hif, .crw, .dng, .tiff, .webp, etc.
- **Videos**: .mp4, .mov, .m4v, .mkv, .avi, .hevc, .3gp, .mts, etc.
- **Audio**: .wav, .mp3
- **Sidecars**: .xmp, .lrf, .aae, .json (metadata files)

### Files to be IGNORED
These file types won't be captured because they're not recognized media:
- Archives: .zip, .7z, .rar, .tar
- Disk images: .dmg, .iso, .bin
- Documents: .txt, .pdf, .doc
- Configuration: .xml, .json, .conf
- Metadata: .ind, .dat

## What If Important Files Are Ignored?

If you see a file type in the **IGNORED** list that actually contains important media:

**Option 1: Add the extension to the scanner**
Edit `/Users/lei/workspace/agents/photo-organizer/main.go` and add to the appropriate map:

```go
var photoExts = map[string]bool{
    ...
    ".myformat": true,  // Add your extension here
}

var videoExts = map[string]bool{
    ...
    ".myvideoformat": true,  // Or here
}
```

Then rebuild: `go build`

**Option 2: Manually copy those files**
If you have unusual media formats, you can:
1. Copy them to a separate folder
2. Run the scan on that folder separately
3. Merge the manifests manually

## Checking for Total Disk Size Match

The report helps you spot if we're missing important files by comparing:

```
Total files on disk ≈ Scanned files + Ignored files
```

**Example:**
- Drive shows: 160.8 GB total content
- Report shows: 160.8 GB scanned + 13.4 MB ignored
- ✓ Close match = we're capturing everything important

**Watch for:** If the actual disk size (from `df` or `Finder`) is significantly larger than the report shows, something might be hidden:
- Hidden files/folders (starting with `.`)
- Symlinks to outside content
- System allocation overhead

Check hidden content with:
```bash
ls -la /Volumes/MyCamera | head -20  # See if anything starts with .
du -sh /Volumes/MyCamera  # Total disk usage
```

## Common File Types by Camera Brand

**Sony Alpha**:
- Photos: .arw (RAW images)
- Videos: .mp4 (stored in PRIVATE/M4ROOT/CLIP/)
- Sidecars: .xml (metadata)

**Canon**:
- Photos: .crw, .cr2, .cr3 (RAW images)
- Videos: .mp4, .mov
- Sidecars: .xmp

**Nikon**:
- Photos: .nef, .nrw (RAW images)
- Videos: .mov, .mp4
- Sidecars: .xmp

**DJI Drones**:
- Videos: .mp4
- Sidecars: .xml, .srt (subtitles)

**iPhone/iPad**:
- Photos: .heic, .jpg
- Videos: .mov
- Sidecars: .jpg (thumbnails)

## Workflow: Safe Scan with Verification

1. **Start the scan** (report shows automatically)
   ```bash
   photo-organizer scan /Volumes/MyDrive -machine "mydrive"
   ```

2. **Review the report** before pressing Enter
   - Check the SCANNED section matches your expectations
   - Check if anything important is in IGNORED
   - Note the total filesize for verification

3. **Cross-check with filesystem** (while report is displayed)
   ```bash
   # In another terminal:
   du -sh /Volumes/MyDrive
   ```

4. **The scan proceeds automatically** after you verify the report

5. **Then verify results**
   ```bash
   photo-organizer analyze
   ```

## Why We Ignore Certain Files

We skip non-media files because:
1. **Performance**: No wasted time hashing .xml, .txt, .conf files
2. **Clarity**: Focus on the media that matters for backup decisions
3. **Accuracy**: Duplicate detection only makes sense for actual media

System folders starting with `.` are skipped because they:
- Contain OS metadata (.fseventsd, .Spotlight-V100)
- Are filesystem internals (.Trashes, recycle bins)
- Would pollute the manifest with non-media files

---

**Need to add custom file types?** Edit `main.go` and rebuild, then commit the change so it's persistent across updates.
