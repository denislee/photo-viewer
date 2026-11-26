package ui

import (
	"sync"

	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// checkImportDirs is importDirsProblem, indirected so tests can stall an
// Apply's validation to race two of them.
var checkImportDirs = importDirsProblem

// pickDir targets — which editor a background directory pick applies to.
const (
	pickInbox = iota
	pickOutbox
)

// importSection: Inbox/Outbox (saved on Apply, after validation), plus the
// delete-source and SD auto-detect toggles (saved on change).
type importSection struct {
	host *SettingsView

	inboxEditor     widget.Editor
	outboxEditor    widget.Editor
	inboxBrowseBtn  widget.Clickable
	outboxBrowseBtn widget.Clickable
	applyBtn        widget.Clickable
	// deleteSource mirrors the import dialog's checkbox (same
	// ImportDeleteSource key; ImportView.Show re-reads it).
	deleteSource widget.Bool
	sdAutoDetect widget.Bool

	mu     sync.Mutex
	status string
	// saveGen invalidates an older Apply's background check/save so it
	// cannot overwrite a newer one's status or config (U5-08). Guarded by mu.
	saveGen int
	// pendingInbox / pendingOutbox hold a directory the background zenity
	// picker (pickDir) chose. The picker can't call editor.SetText itself —
	// that mutates a widget.Editor the frame goroutine is laying out — so it
	// stashes the path here (guarded by mu) and layout applies it on the UI
	// thread. nil means "nothing pending".
	pendingInbox  *string
	pendingOutbox *string
}

func (s *importSection) id() string { return "import" }

func (s *importSection) show(c Config) {
	s.inboxEditor.SetText(c.InboxDir)
	s.outboxEditor.SetText(c.OutboxDir)
	s.deleteSource.Value = c.ImportDeleteSource
	s.sdAutoDetect.Value = c.SDCardAutoDetect
	s.setStatus("")
}

func (s *importSection) statusText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *importSection) setStatus(msg string) {
	s.mu.Lock()
	s.status = msg
	s.mu.Unlock()
}

// apply validates the editors' paths off the UI goroutine and saves them.
// importDirsProblem EvalSymlinks (and may block on an autofs mount or
// spun-down disk), so it runs in a goroutine; saveGen keeps an older click
// from overwriting a newer one.
func (s *importSection) apply() {
	inbox := s.inboxEditor.Text()
	outbox := s.outboxEditor.Text()
	s.mu.Lock()
	s.status = "Saving…"
	s.saveGen++
	gen := s.saveGen
	s.mu.Unlock()
	s.host.redraw()
	go func() {
		p := checkImportDirs(inbox, outbox)
		s.mu.Lock()
		if s.saveGen != gen {
			s.mu.Unlock()
			s.host.redraw()
			return
		}
		if p != "" {
			s.status = "Not saved: " + p
		} else {
			s.status = "Saved"
			// Persist in the SAME critical section as the saveGen check.
			// Releasing s.mu first would leave a window where a newer Apply
			// bumps saveGen and writes its config before this older one calls
			// UpdateConfig, letting the older values win. UpdateConfig only
			// mutates config under cfgMu and never calls back into this view,
			// so holding s.mu across it cannot deadlock.
			UpdateConfig(func(c *Config) {
				c.InboxDir = inbox
				c.OutboxDir = outbox
			})
		}
		s.mu.Unlock()
		s.host.redraw()
	}()
}

func (s *importSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	// Apply any directory the background picker stashed. SetText must run on
	// the UI goroutine — doing it inside pickDir would mutate the editor
	// while this layout reads it.
	s.mu.Lock()
	pInbox, pOutbox := s.pendingInbox, s.pendingOutbox
	s.pendingInbox, s.pendingOutbox = nil, nil
	s.mu.Unlock()
	if pInbox != nil {
		s.inboxEditor.SetText(*pInbox)
	}
	if pOutbox != nil {
		s.outboxEditor.SetText(*pOutbox)
	}
	if s.inboxBrowseBtn.Clicked(gtx) {
		go s.pickDir(pickInbox, "Select inbox directory")
	}
	if s.outboxBrowseBtn.Clicked(gtx) {
		go s.pickDir(pickOutbox, "Select outbox directory")
	}
	if s.applyBtn.Clicked(gtx) {
		s.apply()
	}
	// Toggles persist immediately, so the SD watcher reacts at once.
	if s.deleteSource.Update(gtx) {
		val := s.deleteSource.Value
		UpdateConfig(func(c *Config) { c.ImportDeleteSource = val })
	}
	if s.sdAutoDetect.Update(gtx) {
		val := s.sdAutoDetect.Value
		UpdateConfig(func(c *Config) { c.SDCardAutoDetect = val })
	}

	return column(gtx,
		layout.Rigid(sectionHeader(th, "Import",
			"Import copies new media from the Inbox into dated folders under the Outbox.")),
		vspace(10),
		layout.Rigid(labeledEditor(th, "Inbox directory", &s.inboxEditor, "/path/to/inbox", &s.inboxBrowseBtn)),
		vspace(10),
		layout.Rigid(labeledEditor(th, "Outbox directory", &s.outboxEditor, "/path/to/outbox", &s.outboxBrowseBtn)),
		vspace(8),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(material.Button(th.Theme, &s.applyBtn, "Apply").Layout),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					msg := s.statusText()
					if msg == "" {
						return mutedText(th, "Directory changes take effect after Apply.")(gtx)
					}
					return bodyText(th, msg)(gtx)
				}),
			)
		}),
		vspace(10),
		layout.Rigid(checkbox(th, &s.deleteSource, "Delete source files after a successful import")),
		vspace(4),
		layout.Rigid(checkbox(th, &s.sdAutoDetect, "Suggest import when a USB drive or SD card is connected")),
	)
}

// pickDir shells out to zenity for a directory picker and stashes the result
// for the target editor (pickInbox / pickOutbox). Runs on its own goroutine
// since zenity blocks, so it must not touch the widget.Editor directly — it
// records the path in a pending field that layout applies on the UI thread.
// Falls back to an error status if zenity is missing — the user can still
// type a path.
func (s *importSection) pickDir(target int, title string) {
	path, err := runZenity("--file-selection", "--directory", "--title="+title)
	if err != nil {
		s.setStatus("Picker failed: " + err.Error() + " (type a path instead)")
		s.host.redraw()
		return
	}
	if path == "" {
		return
	}
	s.mu.Lock()
	switch target {
	case pickInbox:
		s.pendingInbox = &path
	case pickOutbox:
		s.pendingOutbox = &path
	}
	s.mu.Unlock()
	s.host.redraw()
}
