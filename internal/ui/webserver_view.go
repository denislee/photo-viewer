package ui

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/webserver"
)

// defaultWebServerPort is used whenever Config.WebServerPort is zero.
const defaultWebServerPort = 8080

// WebServerView is the modal for starting / stopping the embedded HTTP
// server that exposes the index over the network. Lets the user set a
// port, an optional password, and whether to bind to all interfaces.
type WebServerView struct {
	Open    bool
	OnClose func()

	srv *webserver.Server

	portEditor     widget.Editor
	passwordEditor widget.Editor
	bindAll        widget.Bool
	startBtn       widget.Clickable
	stopBtn        widget.Clickable
	closeBtn       widget.Clickable

	mu     sync.Mutex
	status string
}

// NewWebServerView constructs the modal bound to srv. srv may be nil at
// construction time; the modal won't be functional until SetServer is called.
func NewWebServerView(srv *webserver.Server) *WebServerView {
	v := &WebServerView{srv: srv}
	v.portEditor.SingleLine = true
	v.passwordEditor.SingleLine = true
	v.passwordEditor.Mask = '•'
	return v
}

// Show populates the editors from the persisted config and reveals the modal.
// Password is intentionally not persisted — the user re-enters it each session.
func (v *WebServerView) Show() {
	c := GetConfig()
	port := c.WebServerPort
	if port == 0 {
		port = defaultWebServerPort
	}
	v.portEditor.SetText(strconv.Itoa(port))
	v.bindAll.Value = c.WebServerBindAll
	v.Open = true
	v.refreshStatus()
}

// Close hides the modal. The webserver keeps running across opens/closes.
func (v *WebServerView) Close() {
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

// Running reports whether the underlying server is up. Used by window.go to
// reflect state in the toolbar button without reaching into the controller.
func (v *WebServerView) Running() bool {
	return v.srv != nil && v.srv.Running()
}

// refreshStatus rebuilds the status line from the server's current state.
func (v *WebServerView) refreshStatus() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.srv == nil {
		v.status = "Server not initialized."
		return
	}
	if v.srv.Running() {
		v.status = fmt.Sprintf("Running at %s", reachableURLs(v.srv.Addr()))
	} else {
		v.status = "Stopped."
	}
}

// Layout draws the modal.
func (v *WebServerView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.startBtn.Clicked(gtx) {
		v.handleStart()
	}
	if v.stopBtn.Clicked(gtx) {
		v.handleStop()
	}

	drawBackground(gtx, th.Background)

	v.mu.Lock()
	status := v.status
	v.mu.Unlock()
	running := v.Running()

	pad := layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(20), Right: unit.Dp(20)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Web server")
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12),
					"Serve every indexed photo and video over HTTP. Anyone with the URL "+
						"(and the password, if set) will be able to browse and download the library.")
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
			layout.Rigid(v.fieldRow(th, "Port", &v.portEditor, "8080")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(v.fieldRow(th, "Password (optional)", &v.passwordEditor, "leave blank for no auth")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.bindAll,
					"Allow access from other devices on the network (bind 0.0.0.0)")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), status)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(v.actionButton(th, running)),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
				)
			}),
		)
	})
}

// actionButton returns the Start or Stop button depending on the running
// state so the user can only invoke the valid action.
func (v *WebServerView) actionButton(th *Theme, running bool) layout.Widget {
	if running {
		return material.Button(th.Theme, &v.stopBtn, "Stop server").Layout
	}
	return material.Button(th.Theme, &v.startBtn, "Start server").Layout
}

// fieldRow renders a single labeled editor with the same look as Settings.
func (v *WebServerView) fieldRow(th *Theme, label string, ed *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), label)
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				ed.SingleLine = true
				editor := material.Editor(th.Theme, ed, hint)
				editor.Color = th.Foreground
				editor.HintColor = th.Muted
				return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(8)), editor.Layout)
			}),
		)
	}
}

func (v *WebServerView) handleStart() {
	if v.srv == nil {
		v.setStatus("Server not initialized.")
		return
	}
	if v.srv.Running() {
		v.refreshStatus()
		return
	}
	portText := strings.TrimSpace(v.portEditor.Text())
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		v.setStatus("Invalid port — choose a value between 1 and 65535.")
		return
	}
	host := "127.0.0.1"
	if v.bindAll.Value {
		host = "0.0.0.0"
	}
	password := v.passwordEditor.Text()
	if host != "127.0.0.1" && strings.TrimSpace(password) == "" {
		v.setStatus("Password required when binding to all interfaces.")
		return
	}
	if err := v.srv.Start(host, port, password); err != nil {
		v.setStatus("Start failed: " + err.Error())
		return
	}
	// Persist port + bind preference (but never the password).
	c := GetConfig()
	c.WebServerPort = port
	c.WebServerBindAll = v.bindAll.Value
	_ = SaveConfig(c)
	v.refreshStatus()
}

func (v *WebServerView) handleStop() {
	if v.srv == nil {
		return
	}
	if err := v.srv.Stop(); err != nil {
		v.setStatus("Stop failed: " + err.Error())
		return
	}
	v.setStatus("Stopped.")
}

func (v *WebServerView) setStatus(s string) {
	v.mu.Lock()
	v.status = s
	v.mu.Unlock()
}

// reachableURLs renders the bound address as a short list of URLs the user can
// open. For 0.0.0.0 we enumerate the host's non-loopback IPv4 addresses so
// remote devices have a concrete URL to type instead of a placeholder.
func reachableURLs(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host != "0.0.0.0" && host != "::" {
		return "http://" + addr
	}
	var urls []string
	urls = append(urls, "http://127.0.0.1:"+port)
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifa := range ifaces {
			if ifa.Flags&net.FlagUp == 0 || ifa.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := ifa.Addrs()
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					ip4 := ipn.IP.To4()
					if ip4 == nil {
						continue
					}
					urls = append(urls, "http://"+ip4.String()+":"+port)
				}
			}
		}
	}
	return strings.Join(urls, "  ·  ")
}
