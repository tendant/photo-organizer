# Photo Organizer Makefile
# Build, test, and install the photo organizer tool

# Binary name
BINARY_NAME=photo-organizer

# Build directory for cross-platform binaries
DIST_DIR=dist

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Build flags
LDFLAGS=-ldflags "-s -w"

# Version info (can be overridden: make VERSION=1.0.0)
VERSION?=dev

.PHONY: all build build-all clean test install install-skill install-user help deps init
.PHONY: linux linux-amd64 linux-arm64 darwin darwin-amd64 darwin-arm64 windows

# Default target
all: build

# Help target
help:
	@echo "Photo Organizer - Makefile targets:"
	@echo ""
	@echo "  make              - Build for current platform (default)"
	@echo "  make build        - Build for current platform"
	@echo "  make build-all    - Build for all platforms (Linux, macOS, Windows)"
	@echo "  make clean        - Remove built binaries"
	@echo "  make test         - Run tests"
	@echo "  make install      - Install to /usr/local/bin (requires sudo)"
	@echo "  make install-user - Install to ~/bin"
	@echo "  make init         - Initialize photo library directory structure"
	@echo "  make install-skill- Install Claude Code skill to current directory"
	@echo "  make deps         - Download dependencies"
	@echo ""
	@echo "Platform-specific builds:"
	@echo "  make linux        - Build for Linux (amd64 + arm64)"
	@echo "  make darwin       - Build for macOS (amd64 + arm64)"
	@echo "  make windows      - Build for Windows (amd64)"
	@echo ""
	@echo "Examples:"
	@echo "  make clean build          # Clean and rebuild"
	@echo "  make build-all            # Build for all platforms"
	@echo "  make install-user         # Install to ~/bin"

# Download dependencies
deps:
	@echo "Downloading dependencies..."
	@$(GOMOD) download
	@echo "✓ Dependencies downloaded"

# Build for current platform
build: deps
	@echo "Building for current platform..."
	@$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) .
	@echo "✓ Built: ./$(BINARY_NAME)"
	@ls -lh $(BINARY_NAME)

# Build for all platforms
build-all: deps linux darwin windows
	@echo ""
	@echo "✓ All binaries built in $(DIST_DIR)/"
	@ls -lh $(DIST_DIR)/

# Linux builds
linux: linux-amd64 linux-arm64

linux-amd64: deps
	@echo "Building for Linux AMD64..."
	@mkdir -p $(DIST_DIR)
	@GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-amd64 .
	@echo "✓ Built: $(DIST_DIR)/$(BINARY_NAME)-linux-amd64"

linux-arm64: deps
	@echo "Building for Linux ARM64..."
	@mkdir -p $(DIST_DIR)
	@GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-arm64 .
	@echo "✓ Built: $(DIST_DIR)/$(BINARY_NAME)-linux-arm64"

# macOS builds
darwin: darwin-amd64 darwin-arm64

darwin-amd64: deps
	@echo "Building for macOS AMD64..."
	@mkdir -p $(DIST_DIR)
	@GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64 .
	@echo "✓ Built: $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64"

darwin-arm64: deps
	@echo "Building for macOS ARM64 (Apple Silicon)..."
	@mkdir -p $(DIST_DIR)
	@GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64 .
	@echo "✓ Built: $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64"

# Windows build
windows: deps
	@echo "Building for Windows AMD64..."
	@mkdir -p $(DIST_DIR)
	@GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe .
	@echo "✓ Built: $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe"

# Run tests
test:
	@echo "Running tests..."
	@$(GOTEST) -v ./...

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@$(GOCLEAN)
	@rm -f $(BINARY_NAME)
	@rm -rf $(DIST_DIR)
	@echo "✓ Cleaned"

# Install to /usr/local/bin (system-wide, requires sudo)
install: build
	@echo "Installing to /usr/local/bin (requires sudo)..."
	@sudo cp $(BINARY_NAME) /usr/local/bin/
	@sudo chmod +x /usr/local/bin/$(BINARY_NAME)
	@echo "✓ Installed to /usr/local/bin/$(BINARY_NAME)"
	@echo ""
	@echo "You can now run '$(BINARY_NAME)' from anywhere"

# Install to ~/bin (user-specific, no sudo required)
install-user: build
	@echo "Installing to ~/bin..."
	@mkdir -p ~/bin
	@cp $(BINARY_NAME) ~/bin/
	@chmod +x ~/bin/$(BINARY_NAME)
	@echo "✓ Installed to ~/bin/$(BINARY_NAME)"
	@echo ""
	@echo "Make sure ~/bin is in your PATH:"
	@echo '  export PATH="$$HOME/bin:$$PATH"'

# Initialize photo library directory structure
init: build
	@echo "Initializing photo library structure..."
	@./$(BINARY_NAME) --init

# Install Claude Code skill to current directory
install-skill: build
	@echo "Installing Claude Code skill..."
	@./$(BINARY_NAME) --install-skill
	@mkdir -p bin
	@cp $(BINARY_NAME) bin/$(BINARY_NAME)
	@chmod +x bin/$(BINARY_NAME)
	@echo "✓ Copied binary to bin/$(BINARY_NAME)"
	@echo ""
	@echo "Skill installed! Use it in Claude Code with:"
	@echo "  /organize-photos"

# Uninstall from system
uninstall:
	@echo "Uninstalling from /usr/local/bin..."
	@sudo rm -f /usr/local/bin/$(BINARY_NAME)
	@echo "✓ Uninstalled"

# Uninstall from user directory
uninstall-user:
	@echo "Uninstalling from ~/bin..."
	@rm -f ~/bin/$(BINARY_NAME)
	@echo "✓ Uninstalled"
