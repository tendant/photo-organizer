# Photo Organizer

A fast, single-binary tool to organize photos by date.

## Features

- Extracts dates from EXIF metadata
- Parses dates from filenames (DJI, Sony, etc.)
- Organizes into `Originals/YYYY/YYYY-MM-DD/` structure
- Detects and skips duplicates
- Maintains a manifest CSV for tracking
- Zero dependencies after compilation

## Building

Requires Go 1.21+

```bash
# Build for current platform
./build.sh

# Build for all platforms (Linux, Mac, Windows)
./build.sh all
```

## Installation

Copy the binary to your Photos folder or add to PATH:

```bash
# Copy to Photos folder
cp photo-organizer ~/Photos/

# Or install system-wide
sudo cp photo-organizer /usr/local/bin/
```

## Usage

```bash
cd ~/Photos

# Preview what will happen (default - safe)
./photo-organizer

# Actually move the files
./photo-organizer --execute
./photo-organizer -x              # short form

# Execute and update manifest
./photo-organizer -x -m
./photo-organizer --execute --update-manifest

# Use custom root directory
./photo-organizer --root /path/to/photos -x
```

## Expected Folder Structure

```
Photos/
├── Incoming/          ← Drop new photos here
├── Originals/         ← Organized photos go here
│   └── 2025/
│       ├── 2025-01-15/
│       └── ...
├── Exports/           ← Your curated/edited photos
├── _Manifest/         ← Tracking CSV
└── photo-organizer    ← This binary
```

## Workflow

1. Import photos from camera/SD card to `Incoming/`
2. Run `./photo-organizer` to preview (dry-run by default)
3. Run `./photo-organizer -x -m` to organize and update manifest
4. Done!

## Supported Formats

**Photos:** JPG, JPEG, PNG, GIF, HEIC, DNG, ARW, CR2, NEF, RAF

**Videos:** MP4, MOV, AVI, MKV

**Audio:** WAV, MP3 (DJI audio files)

**Sidecars:** LRF, XMP, JSON
