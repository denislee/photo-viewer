package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

func (c *Controller) showSettings() {
	prefs := fyne.CurrentApp().Preferences()
	inbox := widget.NewEntry()
	inbox.SetText(prefs.StringWithFallback("InboxDir", ""))
	outbox := widget.NewEntry()
	outbox.SetText(prefs.StringWithFallback("OutboxDir", ""))

	inboxBrowse := widget.NewButton("Browse", func() {
		dialog.ShowFolderOpen(func(list fyne.ListableURI, err error) {
			if list != nil {
				inbox.SetText(list.Path())
			}
		}, c.window)
	})

	outboxBrowse := widget.NewButton("Browse", func() {
		dialog.ShowFolderOpen(func(list fyne.ListableURI, err error) {
			if list != nil {
				outbox.SetText(list.Path())
			}
		}, c.window)
	})

	form := widget.NewForm(
		widget.NewFormItem("Inbox Directory", container.NewBorder(nil, nil, nil, inboxBrowse, inbox)),
		widget.NewFormItem("Outbox Directory", container.NewBorder(nil, nil, nil, outboxBrowse, outbox)),
	)

	dialog.ShowCustomConfirm("Settings", "Save", "Cancel", form, func(save bool) {
		if save {
			prefs.SetString("InboxDir", inbox.Text)
			prefs.SetString("OutboxDir", outbox.Text)
		}
	}, c.window)
}
