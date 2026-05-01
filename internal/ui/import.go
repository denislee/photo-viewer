package ui

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"

	"github.com/rwcarlsen/goexif/exif"

	"github.com/dns/photo-viewer/internal/scan"
)

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

func (c *Controller) runImport() {
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

	logText := ""

	logEntry := widget.NewMultiLineEntry()
	// Disable typing, but allow text selection and scrolling
	logEntry.Wrapping = fyne.TextWrapWord

	var d dialog.Dialog

	appendLog := func(msg string) {
		fyne.Do(func() {
			logText += msg + "\n"
			logEntry.SetText(logText)
			logEntry.CursorRow = strings.Count(logText, "\n")
			logEntry.Refresh()
		})
	}

	statusLabel := widget.NewLabelWithStyle(fmt.Sprintf("Ready to import %d files.", fileCount), fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	if fileCount == 0 {
		statusLabel.SetText("Ready. Inbox is empty. Select a ZIP file.")
	}
	progressBar := widget.NewProgressBar()
	progressBar.Max = float64(fileCount)
	progressBar.SetValue(0)

	copyBtn := widget.NewButton("Copy to Clipboard", func() {
		c.window.Clipboard().SetContent(logText)
	})
	copyBtn.Disable()

	importAgainBtn := widget.NewButton("Import Again", func() {
		if d != nil {
			d.Hide()
		}
		c.runImport()
	})
	importAgainBtn.Disable()

	var startBtn *widget.Button
	var importZipBtn *widget.Button

	runImportLogic := func(entriesToProcess []string, onComplete func()) {
		appendLog(fmt.Sprintf("Starting import from %s to %s", inboxDir, outboxDir))

		baseDates := make(map[string]time.Time)

		// First pass: find the oldest date for each base name
		for _, srcPath := range entriesToProcess {
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

			// Fallback to file mod time
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

		processed := 0
		// Second pass: move files to the folder of their base name's oldest date
		for _, srcPath := range entriesToProcess {
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

			// Handle duplicate names if necessary
			if _, err := os.Stat(destPath); err == nil {
				ext := filepath.Ext(baseName)
				destPath = filepath.Join(destDirPath, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
			}

			err := os.Rename(srcPath, destPath)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not move %s: %v", baseName, err))
			} else {
				appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, filepath.Join(dateFolder, filepath.Base(destPath))))
			}

			processed++
			fyne.Do(func() {
				progressBar.SetValue(float64(processed))
			})
		}
		appendLog("Import process finished.")
		fyne.Do(func() {
			statusLabel.SetText("Finished importing.")
			if onComplete != nil {
				onComplete()
			} else {
				copyBtn.Enable()
				importAgainBtn.Enable()
			}
		})
	}

	var zipFiles []string
	var importDirs []string

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

	var importDirBtn *widget.Button

	startBtn = widget.NewButton("Start Import", func() {
		startBtn.Disable()
		if importZipBtn != nil {
			importZipBtn.Disable()
		}
		if importDirBtn != nil {
			importDirBtn.Disable()
		}
		statusLabel.SetText("Processing...")

		go func() {
			for _, zipPath := range zipFiles {
				appendLog("Extracting " + zipPath + " ...")
				r, err := zip.OpenReader(zipPath)
				if err != nil {
					appendLog("[ERROR] Failed to open ZIP: " + err.Error())
					continue
				}

				extractedCount := 0
				for _, f := range r.File {
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

					base := filepath.Base(f.Name)
					destPath := filepath.Join(inboxDir, base)
					if _, err := os.Stat(destPath); err == nil {
						ext := filepath.Ext(base)
						name := strings.TrimSuffix(base, ext)
						destPath = filepath.Join(inboxDir, fmt.Sprintf("%s_%d%s", name, time.Now().UnixNano(), ext))
					}

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

			for _, srcDir := range importDirs {
				appendLog("Copying from " + srcDir + " ...")
				copiedCount := 0
				walkErr := filepath.WalkDir(srcDir, func(path string, dEntry fs.DirEntry, err error) error {
					if err != nil || dEntry.IsDir() {
						return nil
					}
					if scan.DetectType(path) == scan.TypeUnknown {
						return nil
					}
					base := filepath.Base(path)
					destPath := filepath.Join(inboxDir, base)
					if _, err := os.Stat(destPath); err == nil {
						ext := filepath.Ext(base)
						name := strings.TrimSuffix(base, ext)
						destPath = filepath.Join(inboxDir, fmt.Sprintf("%s_%d%s", name, time.Now().UnixNano(), ext))
					}
					if err := copyFile(path, destPath); err != nil {
						appendLog("[ERROR] Copy failed for " + path + ": " + err.Error())
						return nil
					}
					copiedCount++
					return nil
				})
				if walkErr != nil {
					appendLog("[ERROR] Walking " + srcDir + ": " + walkErr.Error())
				}
				appendLog(fmt.Sprintf("Copied %d media files from %s to Inbox.", copiedCount, srcDir))
			}

			// Now read the inbox to process files
			var validEntries []string
			walkErr := filepath.WalkDir(inboxDir, func(path string, dEntry fs.DirEntry, err error) error {
				if err != nil || dEntry.IsDir() {
					return nil
				}
				if scan.DetectType(path) != scan.TypeUnknown {
					validEntries = append(validEntries, path)
				}
				return nil
			})
			if walkErr != nil {
				appendLog("[ERROR] Failed to read inbox: " + walkErr.Error())
				fyne.Do(func() { statusLabel.SetText("Import failed.") })
				return
			}

			if len(validEntries) == 0 {
				appendLog("No media files found to import.")
				fyne.Do(func() {
					statusLabel.SetText("Finished. Nothing to import.")
					copyBtn.Enable()
					importAgainBtn.Enable()
				})
				return
			}

			fyne.Do(func() {
				progressBar.Max = float64(len(validEntries))
				progressBar.SetValue(0)
				statusLabel.SetText("Importing...")
			})

			runImportLogic(validEntries, func() {
				if len(zipFiles) > 0 {
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
				} else {
					copyBtn.Enable()
					importAgainBtn.Enable()
				}
			})
		}()
	})
	startBtn.Importance = widget.HighImportance
	if fileCount == 0 {
		startBtn.Disable()
	}

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

	buttons := container.NewHBox(importAgainBtn, startBtn, importZipBtn, importDirBtn, copyBtn)
	topContent := container.NewVBox(statusLabel, progressBar)
	content := container.NewBorder(topContent, buttons, nil, nil, logEntry)

	d = dialog.NewCustom("Import Process", "Close", content, c.window)
	d.Resize(fyne.NewSize(650, 500))
	d.Show()
}
