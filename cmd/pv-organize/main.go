package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

func main() {
	srcDir := flag.String("src", "", "source directory to organize")
	dstDir := flag.String("dst", "", "destination root directory")
	dryRun := flag.Bool("dry-run", false, "show what would be moved without moving")
	flag.Parse()

	if *srcDir == "" || *dstDir == "" {
		fmt.Println("Usage: pv-organize -src <source_dir> -dst <destination_dir> [-dry-run]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if _, err := exec.LookPath("exiftool"); err != nil {
		log.Fatal("Error: exiftool not found on PATH. It is required for metadata extraction.")
	}

	absDst, err := filepath.Abs(*dstDir)
	if err != nil {
		log.Fatalf("Error resolving destination path: %v", err)
	}

	count := 0
	for res := range scan.Walk(context.Background(), *srcDir) {
		if res.Type == scan.TypeUnknown {
			continue
		}

		date, err := getMediaDate(res.Path)
		if err != nil {
			log.Printf("Warning: could not get date for %s: %v. Using file modification time.", res.Path, err)
			date = res.ModTime
		}

		destSubDir := filepath.Join(absDst, date.Format("2006/01/02"))
		destPath := filepath.Join(destSubDir, filepath.Base(res.Path))

		// Avoid moving a file to itself or into its own subdirectory if src and dst overlap
		absSrc, _ := filepath.Abs(res.Path)
		if absSrc == destPath {
			continue
		}

		if *dryRun {
			fmt.Printf("[Dry Run] %s -> %s\n", res.Path, destPath)
			count++
			continue
		}

		if err := os.MkdirAll(destSubDir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v", destSubDir, err)
			continue
		}

		// Handle filename collisions
		finalDest := destPath
		suffix := 1
		for {
			if _, err := os.Stat(finalDest); os.IsNotExist(err) {
				break
			}
			ext := filepath.Ext(destPath)
			base := strings.TrimSuffix(destPath, ext)
			finalDest = fmt.Sprintf("%s_%d%s", base, suffix, ext)
			suffix++
		}

		fmt.Printf("Moving %s -> %s\n", res.Path, finalDest)
		if err := os.Rename(res.Path, finalDest); err != nil {
			log.Printf("Error moving %s: %v", res.Path, err)
		} else {
			count++
		}
	}

	if *dryRun {
		fmt.Printf("\nDry run finished. Would have moved %d files.\n", count)
	} else {
		fmt.Printf("\nFinished. Moved %d files.\n", count)
	}
}

func getMediaDate(path string) (time.Time, error) {
	// Try common date tags. -s -S gives just the value.
	// We use -CreateDate, -DateTimeOriginal, and -MediaCreateDate.
	cmd := exec.Command("exiftool", "-s", "-S", "-CreateDate", "-DateTimeOriginal", "-MediaCreateDate", path)
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// ExifTool format is usually 2024:05:04 12:34:56
		// Sometimes it might include a timezone like +00:00
		t, err := time.Parse("2006:01:02 15:04:05", line)
		if err == nil {
			return t, nil
		}
		// Try with timezone if the above fails
		t, err = time.Parse("2006:01:02 15:04:05-07:00", line)
		if err == nil {
			return t, nil
		}
		t, err = time.Parse("2006:01:02 15:04:05Z", line)
		if err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("no valid date tag found")
}
