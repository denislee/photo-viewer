package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dns/photo-viewer/internal/scan"
)

// maxCollisionSuffix caps how many "_1", "_2", … variants we'll try before
// giving up on a single file. It exists purely as a backstop: the old code had
// no cap and a non-IsNotExist stat error (e.g. EACCES) made it treat the
// destination as a permanent collision and spin forever. With the cap the
// worst case is a single per-file failure, never a hung process.
const maxCollisionSuffix = 10000

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
	failures := 0
	// claimed records destinations the dry-run planner has already handed out
	// this run so two identically-named sources resolve to distinct names —
	// exactly what the real run's atomic claim does when the first source takes
	// the base name and the second collides onto "_1". Without it the old
	// dry-run printed the same destination twice and lied about the outcome.
	claimed := map[string]bool{}

	// SkipDurationProbe: pv-organize only needs each file's path and date, never
	// a video's duration, so suppress the per-video ffprobe fork that a plain
	// Walk would pay for every clip.
	for res := range scan.WalkWith(context.Background(), *srcDir, scan.WalkOptions{SkipDurationProbe: true}) {
		if res.Type == scan.TypeUnknown {
			continue
		}

		// scan.GetMediaDate rides the shared -stay_open exiftool daemon
		// (≈two orders of magnitude faster than a fresh process per file) and
		// already falls back to the file's mtime when no metadata date exists,
		// so the old per-file exec + manual mtime fallback is gone.
		date := scan.GetMediaDate(res.Path)

		destSubDir := filepath.Join(absDst, date.Format("2006/01/02"))
		baseName := filepath.Base(res.Path)
		destPath := filepath.Join(destSubDir, baseName)

		// Avoid moving a file to itself or into its own subdirectory if src and
		// dst overlap: a file already sitting at its correct destination is
		// left untouched (a rename onto its own path would otherwise be pushed
		// to a needless "_1" suffix).
		absSrc, _ := filepath.Abs(res.Path)
		if absSrc == destPath {
			continue
		}

		if *dryRun {
			// Plan the destination with the SAME name sequence and collision
			// rule the real run uses, treating a path as taken if it exists on
			// disk OR we've already planned to fill it this run. This is what
			// makes the printed plan match a real run file-for-file.
			taken := func(p string) bool { return claimed[p] || statTaken(p) }
			finalDest, err := nextFreeName(destSubDir, baseName, taken)
			if err != nil {
				log.Printf("Error planning destination for %s: %v", res.Path, err)
				failures++
				continue
			}
			claimed[finalDest] = true
			fmt.Printf("[Dry Run] %s -> %s\n", res.Path, finalDest)
			count++
			continue
		}

		if err := os.MkdirAll(destSubDir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v", destSubDir, err)
			failures++
			continue
		}

		finalDest, err := moveFile(absSrc, destSubDir, baseName)
		if err != nil {
			log.Printf("Error moving %s: %v", res.Path, err)
			failures++
			continue
		}
		fmt.Printf("Moving %s -> %s\n", res.Path, finalDest)
		count++
	}

	if *dryRun {
		fmt.Printf("\nDry run finished. Would have moved %d files (%d planning error(s)).\n", count, failures)
	} else {
		fmt.Printf("\nFinished. Moved %d files (%d error(s)).\n", count, failures)
	}
	// Mirror pv-export-favorites: any failed operation makes the whole run a
	// failure so scripts and cron jobs don't treat a partial move as success.
	if failures > 0 {
		os.Exit(1)
	}
}

// moveFile moves src into destDir under a collision-free name derived from
// baseName and returns the final path it claimed. It NEVER overwrites an
// existing file and NEVER unlinks src until the destination is durably in
// place. It replaces the old stat-then-rename, which had two data-loss holes:
// (1) anything created in the window between the stat and the rename was
// silently destroyed, and (2) os.Rename returns EXDEV across filesystems — the
// tool's primary use case (SD card to library disk on a different mount) — so
// every move failed and the files stayed stuck at the source.
func moveFile(src, destDir, baseName string) (string, error) {
	for attempt := 0; attempt <= maxCollisionSuffix; attempt++ {
		dst := candidatePath(destDir, baseName, attempt)
		// os.Link is the atomic no-overwrite claim: it creates a second name
		// for src's inode and FAILS with EEXIST if dst already exists, closing
		// the TOCTOU overwrite window (a concurrent run, syncthing, …) the old
		// stat-then-rename left open. On success we drop the source name,
		// completing an atomic move with no window in which the bytes are lost.
		err := os.Link(src, dst)
		switch {
		case err == nil:
			if rmErr := os.Remove(src); rmErr != nil {
				// dst already holds the content (hardlink), so nothing is lost;
				// surface the leftover source so the operator can clean it up.
				return "", fmt.Errorf("linked to %s but could not remove source %s: %w", dst, src, rmErr)
			}
			return dst, nil
		case errors.Is(err, os.ErrExist):
			continue // name taken (possibly a concurrent run) — advance the suffix
		case isCrossDeviceOrUnsupported(err):
			// EXDEV: src and dst are on different filesystems, the tool's
			// primary use (SD card to library disk). EPERM/ENOSYS/…: a source
			// filesystem without hardlink support (FAT). Either way fall back
			// to a crash-safe copy that still refuses to overwrite.
			finalDest, cerr := crossDeviceMove(src, destDir, dst)
			if errors.Is(cerr, os.ErrExist) {
				continue // dst appeared between our attempt and the copy — advance the suffix
			}
			if cerr != nil {
				return "", cerr
			}
			return finalDest, nil
		default:
			// A claim error that ISN'T "already exists" (EACCES, ENOSPC,
			// read-only fs, …). Surface it as a per-file failure rather than
			// looping — the old !IsNotExist-as-collision code spun forever here.
			return "", err
		}
	}
	return "", fmt.Errorf("gave up after %d collisions finding a free name for %q in %s", maxCollisionSuffix, baseName, destDir)
}

// isCrossDeviceOrUnsupported reports whether an os.Link error means the link
// couldn't be created because of where the files live rather than a genuine
// failure: a different filesystem (EXDEV), or a filesystem that doesn't support
// hardlinks at all (FAT/exFAT typically surface EPERM, and some report
// EOPNOTSUPP / ENOSYS / EMLINK). All of these should route to the copy
// fallback instead of failing the file.
func isCrossDeviceOrUnsupported(err error) bool {
	return errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EMLINK)
}

// crossDeviceMove copies src into destDir, atomically places it at dst without
// overwriting, and only then removes src. Used when os.Link can't move the file
// directly (different filesystems, or a source filesystem without hardlinks).
// It mirrors import.go's copyFile crash-safety: stream into a unique temp on
// the destination filesystem, fsync it durable, and place it under the final
// name — so a yanked SD card or power loss mid-copy can never leave a truncated
// file at dst, and src is unlinked ONLY after its bytes are durably in place.
// Returns fs.ErrExist (via syscall.EEXIST) when dst is already taken so the
// caller can advance to the next suffix.
func crossDeviceMove(src, destDir, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	// Unique temp in the destination dir (same filesystem as dst) so two
	// concurrent runs can't clobber each other's partial copy.
	tmp, err := os.CreateTemp(destDir, ".pv-organize-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName) // best-effort: never leave a partial temp behind
		return "", err
	}
	// fsync before the placement + source unlink: a failed Sync is exactly the
	// "bytes still only in the page cache" case, so treat it as a copy failure.
	// Deleting src while dst's bytes weren't yet durable would let a power loss
	// destroy the sole copy.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	if err := placeNoClobber(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return "", err // may be fs.ErrExist → caller advances the suffix
	}
	// Destination is durably in place under its final name; only now is it safe
	// to drop the source, and the move counts as a success only if this unlink
	// succeeds.
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("copied to %s but could not remove source %s: %w", dst, src, err)
	}
	return dst, nil
}

// placeNoClobber moves the fully-written temp file to its final name dst on the
// SAME filesystem without overwriting an existing file. It prefers an atomic
// hardlink (which fails EEXIST on collision, closing the overwrite window
// completely); on the rare destination filesystem without hardlink support it
// falls back to a stat-guarded rename. The residual TOCTOU of that fallback
// applies only to hardlink-less destinations (never a real library disk) and is
// still strictly safer than the old unconditional-overwrite behavior. Returns
// fs.ErrExist when dst is already taken.
func placeNoClobber(tmp, dst string) error {
	err := os.Link(tmp, dst)
	switch {
	case err == nil:
		os.Remove(tmp) // best-effort: dst now holds the content via the hardlink
		return nil
	case errors.Is(err, os.ErrExist):
		return err
	case !isCrossDeviceOrUnsupported(err):
		return err
	}
	// Hardlink-less destination filesystem: guard a plain (atomic, universal)
	// rename with a stat so we still don't clobber a file that appeared.
	if statTaken(dst) {
		return syscall.EEXIST
	}
	return os.Rename(tmp, dst)
}

// candidatePath returns the destination for the given attempt: attempt 0 is the
// bare base name, attempt N appends "_N" before the extension. Shared by the
// real move loop and the dry-run planner so both walk the SAME name sequence.
func candidatePath(destDir, baseName string, attempt int) string {
	if attempt == 0 {
		return filepath.Join(destDir, baseName)
	}
	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	return filepath.Join(destDir, fmt.Sprintf("%s_%d%s", stem, attempt, ext))
}

// nextFreeName walks the candidate sequence and returns the first name for
// which taken() is false, capped at maxCollisionSuffix so a pathological or
// unreadable directory can never spin forever. Used by the dry-run planner so
// its printed destinations match a real run.
func nextFreeName(destDir, baseName string, taken func(string) bool) (string, error) {
	for attempt := 0; attempt <= maxCollisionSuffix; attempt++ {
		dst := candidatePath(destDir, baseName, attempt)
		if !taken(dst) {
			return dst, nil
		}
	}
	return "", fmt.Errorf("gave up after %d collisions finding a free name for %q in %s", maxCollisionSuffix, baseName, destDir)
}

// statTaken reports whether p is unavailable as a destination: either a file
// really exists there, or it can't be stat'd for a reason OTHER than "not
// found" (e.g. EACCES). The latter is treated as taken so the planner advances
// instead of hanging — the old code treated a non-IsNotExist error as a
// permanent collision and looped forever.
func statTaken(p string) bool {
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}
