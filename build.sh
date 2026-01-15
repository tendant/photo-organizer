#!/bin/bash
#
# Build photo-organizer for multiple platforms
#
# Usage:
#   ./build.sh           # Build for current platform
#   ./build.sh all       # Build for Linux, Mac, Windows
#

set -e

cd "$(dirname "$0")"

# Get dependencies
echo "Downloading dependencies..."
go mod download

if [ "$1" = "all" ]; then
    echo "Building for all platforms..."

    # Linux AMD64
    echo "  → linux-amd64"
    GOOS=linux GOARCH=amd64 go build -o dist/photo-organizer-linux-amd64 .

    # Linux ARM64 (Raspberry Pi, etc)
    echo "  → linux-arm64"
    GOOS=linux GOARCH=arm64 go build -o dist/photo-organizer-linux-arm64 .

    # macOS AMD64
    echo "  → darwin-amd64"
    GOOS=darwin GOARCH=amd64 go build -o dist/photo-organizer-darwin-amd64 .

    # macOS ARM64 (M1/M2)
    echo "  → darwin-arm64"
    GOOS=darwin GOARCH=arm64 go build -o dist/photo-organizer-darwin-arm64 .

    # Windows
    echo "  → windows-amd64"
    GOOS=windows GOARCH=amd64 go build -o dist/photo-organizer-windows-amd64.exe .

    echo ""
    echo "Binaries built in dist/"
    ls -lh dist/
else
    echo "Building for current platform..."
    go build -o photo-organizer .
    echo ""
    echo "Built: ./photo-organizer"
    ls -lh photo-organizer
fi

echo ""
echo "Done!"
