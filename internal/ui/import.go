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

	logText := ""
	
	logEntry := widget.NewMultiLineEntry()
	// Disable typing, but allow text selection and scrolling
	logEntry.Wrapping = fyne.TextWrapWord
	
	appendLog := func(msg string) {
		logText += msg + "\n"
		logEntry.SetText(logText)
		// MultiLineEntry will automatically update its display
	}

	copyBtn := widget.NewButton("Copy to Clipboard", func() {
		c.window.Clipboard().SetContent(logText)
	})

	content := container.NewBorder(nil, copyBtn, nil, nil, logEntry)
	
	d := dialog.NewCustom("Import Process", "Close", content, c.window)
	d.Resize(fyne.NewSize(650, 500))
	d.Show()

	go func() {
		appendLog(fmt.Sprintf("Starting import from %s to %s", inboxDir, outboxDir))
		
		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			srcPath := filepath.Join(inboxDir, e.Name())
			file, err := os.Open(srcPath)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not open %s: %v", e.Name(), err))
				continue
			}

			// Try to get EXIF
			var mediaDate time.Time
			x, err := exif.Decode(file)
			if err == nil {
				tm, err := x.DateTime()
				if err == nil {
					mediaDate = tm
				}
			}
			file.Close()

			// Fallback to file mod time
			if mediaDate.IsZero() {
				info, err := e.Info()
				if err == nil {
					mediaDate = info.ModTime()
				} else {
					mediaDate = time.Now()
				}
			}

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
				base := strings.TrimSuffix(e.Name(), ext)
				destPath = filepath.Join(destDirPath, fmt.Sprintf("%s_%d%s", base, time.Now().Unix(), ext))
			}

			err = os.Rename(srcPath, destPath)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Could not move %s: %v", e.Name(), err))
			} else {
				appendLog(fmt.Sprintf("[OK] Moved %s -> %s", e.Name(), filepath.Join(dateFolder, filepath.Base(destPath))))
			}
		}
		appendLog("Import process finished.")
	}()
}
