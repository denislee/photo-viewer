package ui

import (
	"archive/zip"
	"fmt"
	"io"
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
)

func (c *Controller) runImport() {
	prefs := fyne.CurrentApp().Preferences()
	inboxDir := prefs.StringWithFallback("InboxDir", "")
	outboxDir := prefs.StringWithFallback("OutboxDir", "")

	if inboxDir == "" || outboxDir == "" {
		dialog.ShowInformation("Error", "Please configure Inbox and Outbox directories in Settings first.", c.window)
		return
	}

	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		dialog.ShowError(err, c.window)
		return
	}

	fileCount := 0
	for _, e := range entries {
		if !e.IsDir() {
			fileCount++
		}
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

	runImportLogic := func(entriesToProcess []os.DirEntry, onComplete func()) {
		appendLog(fmt.Sprintf("Starting import from %s to %s", inboxDir, outboxDir))

		baseDates := make(map[string]time.Time)

		// First pass: find the oldest date for each base name
		for _, e := range entriesToProcess {
			srcPath := filepath.Join(inboxDir, e.Name())
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
				info, err := e.Info()
				if err == nil {
					mediaDate = info.ModTime()
				} else {
					mediaDate = time.Now()
				}
			}

			base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if existingDate, ok := baseDates[base]; !ok || mediaDate.Before(existingDate) {
				baseDates[base] = mediaDate
			}
		}

		processed := 0
		// Second pass: move files to the folder of their base name's oldest date
		for _, e := range entriesToProcess {
			srcPath := filepath.Join(inboxDir, e.Name())
			base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			mediaDate := baseDates[base]

			dateFolder := mediaDate.Format("2006-01-02")
			destDirPath := filepath.Join(outboxDir, dateFolder)
			if err := os.MkdirAll(destDirPath, 0755); err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not create directory %s: %v", destDirPath, err))
				continue
			}

			destPath := filepath.Join(destDirPath, e.Name())

			// Handle duplicate names if necessary
			if _, err := os.Stat(destPath); err == nil {
				ext := filepath.Ext(e.Name())
				destPath = filepath.Join(destDirPath, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
			}

			err := os.Rename(srcPath, destPath)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not move %s: %v", e.Name(), err))
			} else {
				appendLog(fmt.Sprintf("[OK] Moved %s -> %s", e.Name(), filepath.Join(dateFolder, filepath.Base(destPath))))
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

	startBtn = widget.NewButton("Start Import", func() {
		startBtn.Disable()
		if importZipBtn != nil {
			importZipBtn.Disable()
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
				appendLog(fmt.Sprintf("Extracted %d files from %s to Inbox.", extractedCount, filepath.Base(zipPath)))
			}

			// Now read the inbox to process files
			entries, err := os.ReadDir(inboxDir)
			if err != nil {
				appendLog("[ERROR] Failed to read inbox: " + err.Error())
				fyne.Do(func() { statusLabel.SetText("Import failed.") })
				return
			}
			var validEntries []os.DirEntry
			for _, e := range entries {
				if !e.IsDir() {
					validEntries = append(validEntries, e)
				}
			}

			if len(validEntries) == 0 {
				appendLog("No files found to import.")
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

			alreadyAdded := false
			for _, z := range zipFiles {
				if z == zipPath {
					alreadyAdded = true
					break
				}
			}

			if !alreadyAdded {
				zipFiles = append(zipFiles, zipPath)
				fyne.Do(func() {
					if len(zipFiles) == 1 {
						statusLabel.SetText(fmt.Sprintf("Ready. %d files in Inbox, 1 ZIP selected.", fileCount))
					} else {
						statusLabel.SetText(fmt.Sprintf("Ready. %d files in Inbox, %d ZIPs selected.", fileCount, len(zipFiles)))
					}
					startBtn.Enable()
				})
			}
		}, c.window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".zip"}))
		fd.Show()
	})

	buttons := container.NewHBox(startBtn, importZipBtn, copyBtn, importAgainBtn)
	topContent := container.NewVBox(statusLabel, progressBar)
	content := container.NewBorder(topContent, buttons, nil, nil, logEntry)

	d = dialog.NewCustom("Import Process", "Close", content, c.window)
	d.Resize(fyne.NewSize(650, 500))
	d.Show()
}
