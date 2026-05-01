package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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

	if fileCount == 0 {
		dialog.ShowInformation("Import", "No files to import in Inbox.", c.window)
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
	startBtn = widget.NewButton("Start Import", func() {
		startBtn.Disable()
		statusLabel.SetText("Importing...")

		go func() {
			appendLog(fmt.Sprintf("Starting import from %s to %s", inboxDir, outboxDir))

			baseDates := make(map[string]time.Time)

			// First pass: find the oldest date for each base name
			for _, e := range entries {
				if e.IsDir() {
					continue
				}

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
			for _, e := range entries {
				if e.IsDir() {
					continue
				}

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
				copyBtn.Enable()
				importAgainBtn.Enable()
			})
		}()
	})
	startBtn.Importance = widget.HighImportance

	buttons := container.NewHBox(startBtn, copyBtn, importAgainBtn)
	topContent := container.NewVBox(statusLabel, progressBar)
	content := container.NewBorder(topContent, buttons, nil, nil, logEntry)

	d = dialog.NewCustom("Import Process", "Close", content, c.window)
	d.Resize(fyne.NewSize(650, 500))
	d.Show()
}
