package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func readManifest(csvPath string) (ManifestSource, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return ManifestSource{}, fmt.Errorf("open %s: %w", csvPath, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return ManifestSource{}, fmt.Errorf("read %s: %w", csvPath, err)
	}
	if len(records) < 1 {
		return ManifestSource{}, fmt.Errorf("%s: empty file", csvPath)
	}

	// Build column index by name so we handle old/new formats gracefully.
	colIdx := make(map[string]int)
	for i, name := range records[0] {
		colIdx[name] = i
	}

	// Validate that required columns are present.
	for _, required := range []string{"relative_path", "partial_hash", "file_size_bytes"} {
		if _, ok := colIdx[required]; !ok {
			// Also accept file_hash as fallback for partial_hash in very old manifests.
			if required == "partial_hash" {
				if _, ok := colIdx["file_hash"]; ok {
					continue
				}
			}
			return ManifestSource{}, fmt.Errorf("%s: missing required column %q (not a valid manifest?)", csvPath, required)
		}
	}

	col := func(row []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}

	var rows []ManifestRow
	var validationIssues []string
	seenPaths := make(map[string]bool)
	skippedCount := 0

	for rowIdx, row := range records[1:] {
		if len(row) < 2 {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: not enough columns", rowIdx+2))
			continue
		}

		relPath := col(row, "relative_path")
		sizeStr := col(row, "file_size_bytes")
		scanDate := col(row, "scan_date")

		if relPath == "" {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: empty relative_path", rowIdx+2))
			continue
		}

		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || size < 0 {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: invalid size %q", rowIdx+2, sizeStr))
			continue
		}

		if scanDate != "" {
			if len(scanDate) < len("2006-01-02") {
				validationIssues = append(validationIssues, fmt.Sprintf("row %d: invalid scan_date %q", rowIdx+2, scanDate))
				skippedCount++
				continue
			}
			scanTime, err := time.Parse("2006-01-02", scanDate[:10])
			if err != nil {
				validationIssues = append(validationIssues, fmt.Sprintf("row %d: invalid scan_date %q", rowIdx+2, scanDate))
				skippedCount++
				continue
			}
			if scanTime.After(time.Now().AddDate(0, 0, 1)) {
				validationIssues = append(validationIssues, fmt.Sprintf("row %d: future scan_date %s (skipped - breaks age calculation)", rowIdx+2, scanDate))
				skippedCount++
				continue
			}
		}

		dupKey := filepath.Join(col(row, "scan_path"), relPath)
		if seenPaths[dupKey] {
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: duplicate entry %s", rowIdx+2, relPath))
			skippedCount++
			continue
		}
		seenPaths[dupKey] = true

		partialHash := col(row, "partial_hash")
		if partialHash == "" {
			partialHash = col(row, "file_hash")
		}

		rows = append(rows, ManifestRow{
			Filename:     col(row, "filename"),
			RelativePath: relPath,
			SizeBytes:    size,
			PartialHash:  partialHash,
			FullHash:     col(row, "full_hash"),
			Extension:    col(row, "extension"),
			FileModified: col(row, "file_modified"),
			ScanDate:     scanDate,
			ScanPath:     col(row, "scan_path"),
			MachineName:  col(row, "machine_name"),
		})
	}

	if skippedCount > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %s: skipped %d invalid row(s)\n", filepath.Base(csvPath), skippedCount)
		if len(validationIssues) > 0 && len(validationIssues) <= 3 {
			for _, issue := range validationIssues {
				fmt.Fprintf(os.Stderr, "   %s\n", issue)
			}
		}
	}

	machine := ""
	scanPath := ""
	lastScanned := ""
	for _, row := range rows {
		if row.MachineName != "" {
			machine = row.MachineName
		}
		if row.ScanPath != "" {
			scanPath = row.ScanPath
		}
		if row.ScanDate > lastScanned {
			lastScanned = row.ScanDate
		}
		if machine != "" && scanPath != "" && lastScanned != "" {
			break
		}
	}
	for _, row := range rows {
		if row.ScanDate > lastScanned {
			lastScanned = row.ScanDate
		}
	}
	if machine == "" {
		stem := strings.TrimSuffix(filepath.Base(csvPath), filepath.Ext(csvPath))
		stem = strings.TrimPrefix(stem, "photo_manifest_")
		stem = strings.TrimPrefix(stem, "photo_manifest")
		if parts := strings.SplitN(stem, "_", 2); len(parts) >= 1 && parts[0] != "" {
			machine = parts[0]
		} else {
			machine = filepath.Base(csvPath)
		}
	}

	label := machine
	if scanPath != "" {
		label = machine + " @ " + scanPath
	}

	for i := range rows {
		if rows[i].MachineName == "" {
			rows[i].MachineName = machine
		}
	}

	if lastScanned != "" {
		if scanTime, err := time.Parse("2006-01-02", lastScanned[:10]); err == nil {
			daysOld := int(time.Since(scanTime).Hours() / 24)
			if daysOld > 30 {
				fmt.Fprintf(os.Stderr, "⚠  %s: manifest is %d days old (last scanned %s)\n", label, daysOld, lastScanned)
			}
		}
	}

	return ManifestSource{
		FilePath:    csvPath,
		MachineName: machine,
		ScanPath:    scanPath,
		Label:       label,
		LastScanned: lastScanned,
		Rows:        rows,
	}, nil
}
