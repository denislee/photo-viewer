package ui

import (
	"os/exec"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// externalOpenCmds builds the external-viewer commands for entries: one mpv
// looping over them when they are all videos, otherwise one xdg-open per file
// (the desktop's default app, which also handles mixed selections).
func externalOpenCmds(entries []cache.Entry) []*exec.Cmd {
	if len(entries) == 0 {
		return nil
	}
	paths := make([]string, len(entries))
	allVideos := true
	for i, e := range entries {
		paths[i] = e.Path
		if e.Type != scan.TypeVideo {
			allVideos = false
		}
	}
	if allVideos {
		args := []string{"--loop", paths[0]}
		if len(paths) > 1 {
			args = append([]string{"--loop-playlist=inf"}, paths...)
		}
		return []*exec.Cmd{exec.Command("mpv", args...)}
	}
	cmds := make([]*exec.Cmd, len(paths))
	for i, p := range paths {
		cmds[i] = exec.Command("xdg-open", p)
	}
	return cmds
}

// openExternally launches externalOpenCmds(entries) in the background.
func openExternally(entries []cache.Entry) {
	for _, cmd := range externalOpenCmds(entries) {
		go runDetached(cmd)
	}
}

// runDetached starts cmd and waits for it to finish so the OS can reap the
// child process. Errors are intentionally discarded — these are fire-and-
// forget launches of external viewers (mpv, xdg-open). Always call from a
// goroutine, since Wait blocks until the child exits.
func runDetached(cmd *exec.Cmd) {
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Wait()
}
