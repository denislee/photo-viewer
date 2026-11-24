package ui

import (
	"fmt"
	"log"
	"os/exec"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// maxExternalOpens caps how many xdg-open processes one gesture launches. A
// large selection would otherwise spawn one desktop app window per file with
// no prompt. The all-videos case is a single mpv playlist and is not capped.
const maxExternalOpens = 20

// externalOpenCmds builds the external-viewer commands for entries: one mpv
// looping over them when they are all videos, otherwise one xdg-open per file
// (the desktop's default app, which also handles mixed selections), for at
// most maxExternalOpens files.
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
	cmds := make([]*exec.Cmd, min(len(paths), maxExternalOpens))
	for i, p := range paths[:len(cmds)] {
		cmds[i] = exec.Command("xdg-open", p)
	}
	return cmds
}

// openExternally launches externalOpenCmds(entries) in the background and
// tells the user when the maxExternalOpens cap left files unopened.
func openExternally(entries []cache.Entry) {
	cmds := externalOpenCmds(entries)
	for _, cmd := range cmds {
		go runDetached(cmd)
	}
	if len(cmds) > 1 && len(cmds) < len(entries) {
		msg := fmt.Sprintf("Opened the first %d of %d selected files; the other %d were skipped.",
			len(cmds), len(entries), len(entries)-len(cmds))
		log.Printf("open externally: %s", msg)
		go warnUser("Open externally", msg)
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
