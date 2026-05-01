package ui

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"

	"github.com/rwcarlsen/goexif/exif"

	"github.com/dns/photo-viewer/internal/scan"
)

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sameContent reports whether a and b have identical bytes. Size mismatch
// short-circuits before hashing.
func sameContent(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ha, err := sha256File(a)
	if err != nil {
		return false, err
	}
	hb, err := sha256File(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// uniqueInboxPath returns a path inside dir that does not yet exist, by
// appending an incrementing suffix to the base name on collision. This
// guarantees imports never overwrite an existing inbox file.
func uniqueInboxPath(dir, base string) string {
	p := filepath.Join(dir, base)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, i, ext))
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

func (c *Controller) runImport() {
	c.runImportFromDirs(nil)
}

// importLogVisibleLines caps how many trailing lines are kept in the visible
// log widget. The full log is still preserved for the clipboard copy.
const importLogVisibleLines = 1000

// importUITickInterval is the cadence at which buffered log lines and the
// progress bar value are pushed to the UI thread during an import. Per-file
// fyne.Do calls are too expensive at high file counts.
const importUITickInterval = 200 * time.Millisecond

// runImportFromDirs opens the import dialog with an optional list of source
// directories already added — used by the SD-card flow to hand off a freshly
// mounted volume.
func (c *Controller) runImportFromDirs(initialDirs []string) {
	prefs := fyne.CurrentApp().Preferences()
	inboxDir := prefs.StringWithFallback("InboxDir", "")
	outboxDir := prefs.StringWithFallback("OutboxDir", "")

	if inboxDir == "" || outboxDir == "" {
		dialog.ShowInformation("Error", "Please configure Inbox and Outbox directories in Settings first.", c.window)
		return
	}

	fileCount := 0
	err := filepath.WalkDir(inboxDir, func(path string, dEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil || dEntry.IsDir() {
			return nil
		}
		if scan.DetectType(path) != scan.TypeUnknown {
			fileCount++
		}
		return nil
	})
	if err != nil {
		dialog.ShowError(err, c.window)
		return
	}

	logEntry := widget.NewMultiLineEntry()
	logEntry.Wrapping = fyne.TextWrapWord

	// Log writes are buffered; a ticker drains them onto the UI thread so a
	// large import doesn't trigger one widget refresh per file.
	var (
		logMu       sync.Mutex
		logPending  []string
		logVisible  []string
		logFullCopy strings.Builder
	)

	appendLog := func(msg string) {
		logMu.Lock()
		logPending = append(logPending, msg)
		logFullCopy.WriteString(msg)
		logFullCopy.WriteByte('\n')
		logMu.Unlock()
	}

	// flushLog must run on the UI thread.
	flushLog := func() {
		logMu.Lock()
		if len(logPending) == 0 {
			logMu.Unlock()
			return
		}
		logVisible = append(logVisible, logPending...)
		logPending = logPending[:0]
		if len(logVisible) > importLogVisibleLines {
			logVisible = logVisible[len(logVisible)-importLogVisibleLines:]
		}
		text := strings.Join(logVisible, "\n")
		rows := len(logVisible)
		logMu.Unlock()
		logEntry.SetText(text)
		logEntry.CursorRow = rows
		logEntry.Refresh()
	}

	statusLabel := widget.NewLabelWithStyle(fmt.Sprintf("Ready to import %d files.", fileCount), fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	if fileCount == 0 {
		statusLabel.SetText("Ready. Inbox is empty. Select a ZIP file.")
	}
	progressBar := widget.NewProgressBar()
	progressBar.Max = float64(fileCount)
	progressBar.SetValue(0)

	var (
		progressMax atomic.Int64
		progressVal atomic.Int64
	)
	progressMax.Store(int64(fileCount))

	var d dialog.Dialog

	copyBtn := widget.NewButton("Copy to Clipboard", func() {
		logMu.Lock()
		s := logFullCopy.String()
		logMu.Unlock()
		c.window.Clipboard().SetContent(s)
	})
	copyBtn.Disable()

	importAgainBtn := widget.NewButton("Import Again", func() {
		if d != nil {
			d.Hide()
		}
		c.runImport()
	})
	importAgainBtn.Disable()

	var (
		startBtn     *widget.Button
		importZipBtn *widget.Button
		importDirBtn *widget.Button
		cancelBtn    *widget.Button
	)

	var (
		ctxMu        sync.Mutex
		importCtx    context.Context
		cancelImport context.CancelFunc
	)

	currentCtx := func() context.Context {
		ctxMu.Lock()
		defer ctxMu.Unlock()
		if importCtx == nil {
			return context.Background()
		}
		return importCtx
	}

	processBatch := func(entriesToProcess []string) {
		ctx := currentCtx()
		baseDates := make(map[string]time.Time)

		// First pass: find the oldest date for each base name.
		for _, srcPath := range entriesToProcess {
			if ctx.Err() != nil {
				return
			}
			var mediaDate time.Time

			file, err := os.Open(srcPath)
			if err == nil {
				x, err := exif.Decode(file)
				if err == nil {
					tm, err := x.DateTime()
					if err == nil {
						mediaDate = tm
					}
				}
				file.Close()
			}

			if mediaDate.IsZero() {
				info, err := os.Stat(srcPath)
				if err == nil {
					mediaDate = info.ModTime()
				} else {
					mediaDate = time.Now()
				}
			}

			baseName := filepath.Base(srcPath)
			base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
			if existingDate, ok := baseDates[base]; !ok || mediaDate.Before(existingDate) {
				baseDates[base] = mediaDate
			}
		}

		// Second pass: move files to the folder of their base name's oldest date.
		for _, srcPath := range entriesToProcess {
			if ctx.Err() != nil {
				return
			}
			baseName := filepath.Base(srcPath)
			base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
			mediaDate := baseDates[base]

			dateFolder := mediaDate.Format("2006-01-02")
			destDirPath := filepath.Join(outboxDir, dateFolder)
			if err := os.MkdirAll(destDirPath, 0755); err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not create directory %s: %v", destDirPath, err))
				continue
			}

			destPath := filepath.Join(destDirPath, baseName)

			// Handle duplicate names: if the existing file has identical content,
			// the source has already been imported — drop it. Otherwise, keep
			// both by appending a unique suffix.
			if _, err := os.Stat(destPath); err == nil {
				same, cmpErr := sameContent(srcPath, destPath)
				if cmpErr != nil {
					appendLog(fmt.Sprintf("[ERROR] Could not compare %s with existing file: %v", baseName, cmpErr))
				}
				if same {
					if rmErr := os.Remove(srcPath); rmErr != nil {
						appendLog(fmt.Sprintf("[ERROR] Could not remove duplicate %s: %v", baseName, rmErr))
					} else {
						appendLog(fmt.Sprintf("[SKIP] %s already in %s (identical)", baseName, dateFolder))
					}
					progressVal.Add(1)
					continue
				}
				ext := filepath.Ext(baseName)
				destPath = filepath.Join(destDirPath, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
			}

			err := os.Rename(srcPath, destPath)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not move %s: %v", baseName, err))
			} else {
				appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, filepath.Join(dateFolder, filepath.Base(destPath))))
			}

			progressVal.Add(1)
		}
	}

	var zipFiles []string
	importDirs := append([]string(nil), initialDirs...)

	updateReadyStatus := func() {
		parts := []string{fmt.Sprintf("%d files in Inbox", fileCount)}
		if len(zipFiles) == 1 {
			parts = append(parts, "1 ZIP")
		} else if len(zipFiles) > 1 {
			parts = append(parts, fmt.Sprintf("%d ZIPs", len(zipFiles)))
		}
		if len(importDirs) == 1 {
			parts = append(parts, "1 directory")
		} else if len(importDirs) > 1 {
			parts = append(parts, fmt.Sprintf("%d directories", len(importDirs)))
		}
		statusLabel.SetText("Ready. " + strings.Join(parts, ", ") + ".")
	}

	startBtn = widget.NewButton("Start Import", func() {
		startBtn.Disable()
		if importZipBtn != nil {
			importZipBtn.Disable()
		}
		if importDirBtn != nil {
			importDirBtn.Disable()
		}
		cancelBtn.Enable()
		statusLabel.SetText("Processing...")

		ctxMu.Lock()
		importCtx, cancelImport = context.WithCancel(context.Background())
		ctx := importCtx
		ctxMu.Unlock()

		tickerStop := make(chan struct{})
		go func() {
			t := time.NewTicker(importUITickInterval)
			defer t.Stop()
			for {
				select {
				case <-tickerStop:
					return
				case <-t.C:
					fyne.Do(func() {
						flushLog()
						progressBar.Max = float64(progressMax.Load())
						progressBar.SetValue(float64(progressVal.Load()))
					})
				}
			}
		}()

		go func() {
			defer func() {
				close(tickerStop)
				fyne.Do(func() {
					flushLog()
					progressBar.Max = float64(progressMax.Load())
					progressBar.SetValue(float64(progressVal.Load()))
				})
			}()

			appendLog(fmt.Sprintf("Starting import to %s", outboxDir))

			// Phase 1: extract ZIPs into the inbox.
			for _, zipPath := range zipFiles {
				if ctx.Err() != nil {
					break
				}
				appendLog("Extracting " + zipPath + " ...")
				r, err := zip.OpenReader(zipPath)
				if err != nil {
					appendLog("[ERROR] Failed to open ZIP: " + err.Error())
					continue
				}

				extractedCount := 0
				for _, f := range r.File {
					if ctx.Err() != nil {
						break
					}
					if f.FileInfo().IsDir() {
						continue
					}
					if scan.DetectType(f.Name) == scan.TypeUnknown {
						continue
					}
					rc, err := f.Open()
					if err != nil {
						appendLog("[ERROR] Failed to open file in ZIP: " + err.Error())
						continue
					}

					destPath := uniqueInboxPath(inboxDir, filepath.Base(f.Name))

					outFile, err := os.Create(destPath)
					if err != nil {
						appendLog("[ERROR] Failed to create file: " + err.Error())
						rc.Close()
						continue
					}
					_, err = io.Copy(outFile, rc)
					outFile.Close()
					rc.Close()
					if err != nil {
						appendLog("[ERROR] Failed to extract file: " + err.Error())
					} else {
						extractedCount++
					}
				}
				r.Close()
				appendLog(fmt.Sprintf("Extracted %d media files from %s to Inbox.", extractedCount, filepath.Base(zipPath)))
			}

			processedAny := false

			// Phase 2: process whatever is currently in the inbox (pre-existing
			// files plus anything just extracted from ZIPs) as one batch.
			if ctx.Err() == nil {
				var inboxEntries []string
				if walkErr := filepath.WalkDir(inboxDir, func(path string, dEntry fs.DirEntry, err error) error {
					if err != nil || dEntry.IsDir() {
						return nil
					}
					if scan.DetectType(path) != scan.TypeUnknown {
						inboxEntries = append(inboxEntries, path)
					}
					return nil
				}); walkErr != nil {
					appendLog("[ERROR] Failed to read inbox: " + walkErr.Error())
				}
				if len(inboxEntries) > 0 {
					appendLog(fmt.Sprintf("Processing %d files already in Inbox...", len(inboxEntries)))
					progressMax.Store(int64(len(inboxEntries)))
					progressVal.Store(0)
					n := len(inboxEntries)
					fyne.Do(func() {
						statusLabel.SetText(fmt.Sprintf("Importing %d inbox files...", n))
					})
					processBatch(inboxEntries)
					processedAny = true
				}
			}

			// Phase 3: for each chosen directory, copy and process one
			// subdirectory at a time so the inbox never holds the whole tree.
			for _, srcDir := range importDirs {
				if ctx.Err() != nil {
					break
				}
				appendLog("Scanning " + srcDir + " ...")
				subdirFiles := map[string][]string{}
				var subdirOrder []string
				walkErr := filepath.WalkDir(srcDir, func(path string, dEntry fs.DirEntry, err error) error {
					if err != nil || dEntry.IsDir() {
						return nil
					}
					if scan.DetectType(path) == scan.TypeUnknown {
						return nil
					}
					parent := filepath.Dir(path)
					if _, ok := subdirFiles[parent]; !ok {
						subdirOrder = append(subdirOrder, parent)
					}
					subdirFiles[parent] = append(subdirFiles[parent], path)
					return nil
				})
				if walkErr != nil {
					appendLog("[ERROR] Walking " + srcDir + ": " + walkErr.Error())
				}

				for _, subdir := range subdirOrder {
					if ctx.Err() != nil {
						break
					}
					files := subdirFiles[subdir]
					appendLog(fmt.Sprintf("Copying %d files from %s to Inbox...", len(files), subdir))
					var batch []string
					for _, src := range files {
						if ctx.Err() != nil {
							break
						}
						destPath := uniqueInboxPath(inboxDir, filepath.Base(src))
						if err := copyFile(src, destPath); err != nil {
							appendLog("[ERROR] Copy failed for " + src + ": " + err.Error())
							continue
						}
						batch = append(batch, destPath)
					}
					if len(batch) == 0 {
						continue
					}
					progressMax.Store(int64(len(batch)))
					progressVal.Store(0)
					n := len(batch)
					sd := subdir
					fyne.Do(func() {
						statusLabel.SetText(fmt.Sprintf("Importing %d files from %s...", n, filepath.Base(sd)))
					})
					processBatch(batch)
					processedAny = true
				}
			}

			cancelled := ctx.Err() != nil
			if cancelled {
				appendLog("Import cancelled.")
			} else {
				appendLog("Import process finished.")
			}
			fyne.Do(func() {
				cancelBtn.Disable()
				if cancelled {
					statusLabel.SetText("Cancelled.")
				} else if processedAny {
					statusLabel.SetText("Finished importing.")
				} else {
					statusLabel.SetText("Finished. Nothing to import.")
				}
			})

			finish := func() {
				fyne.Do(func() {
					copyBtn.Enable()
					importAgainBtn.Enable()
				})
			}

			if !cancelled && len(zipFiles) > 0 {
				fyne.Do(func() {
					msg := fmt.Sprintf("Import is complete.\nDo you want to delete the %d original ZIP files?", len(zipFiles))
					if len(zipFiles) == 1 {
						msg = fmt.Sprintf("Import is complete.\nDo you want to delete the original ZIP file (%s)?", filepath.Base(zipFiles[0]))
					}
					dialog.ShowConfirm("Delete ZIP(s)?", msg, func(del bool) {
						if del {
							for _, z := range zipFiles {
								if err := os.Remove(z); err != nil {
									appendLog("[ERROR] Failed to delete ZIP: " + err.Error())
									dialog.ShowError(err, c.window)
								} else {
									appendLog("[OK] Deleted ZIP file: " + z)
								}
							}
						}
						copyBtn.Enable()
						importAgainBtn.Enable()
					}, c.window)
				})
			} else {
				finish()
			}
		}()
	})
	startBtn.Importance = widget.HighImportance
	if fileCount == 0 && len(importDirs) == 0 {
		startBtn.Disable()
	}
	if len(importDirs) > 0 {
		updateReadyStatus()
	}

	cancelBtn = widget.NewButton("Cancel", func() {
		ctxMu.Lock()
		cancel := cancelImport
		ctxMu.Unlock()
		if cancel != nil {
			cancel()
		}
		cancelBtn.Disable()
		statusLabel.SetText("Cancelling...")
	})
	cancelBtn.Disable()

	importZipBtn = widget.NewButton("Add ZIP", func() {
		fd := dialog.NewFileOpen(func(uc fyne.URIReadCloser, err error) {
			if err != nil || uc == nil {
				return
			}
			zipPath := uc.URI().Path()
			uc.Close()

			for _, z := range zipFiles {
				if z == zipPath {
					return
				}
			}

			zipFiles = append(zipFiles, zipPath)
			fyne.Do(func() {
				updateReadyStatus()
				startBtn.Enable()
			})
		}, c.window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".zip"}))
		fd.Show()
	})

	importDirBtn = widget.NewButton("Add Directory", func() {
		dialog.ShowFolderOpen(func(list fyne.ListableURI, err error) {
			if err != nil || list == nil {
				return
			}
			dirPath := list.Path()
			for _, p := range importDirs {
				if p == dirPath {
					return
				}
			}
			importDirs = append(importDirs, dirPath)
			fyne.Do(func() {
				updateReadyStatus()
				startBtn.Enable()
			})
		}, c.window)
	})

	buttons := container.NewHBox(importAgainBtn, startBtn, cancelBtn, importZipBtn, importDirBtn, copyBtn)
	topContent := container.NewVBox(statusLabel, progressBar)
	content := container.NewBorder(topContent, buttons, nil, nil, logEntry)

	d = dialog.NewCustom("Import Process", "Close", content, c.window)
	d.Resize(fyne.NewSize(650, 500))
	d.Show()
}
