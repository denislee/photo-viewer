package ui

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestExternalOpenCmds is the G-12 guard: O used to launch mpv for all-video
// selections and silently do nothing for photos or mixed ones. Photos and
// mixed selections now go to xdg-open, one process per file.
func TestExternalOpenCmds(t *testing.T) {
	vid := func(p string) cache.Entry { return cache.Entry{Path: p, Type: scan.TypeVideo} }
	photo := func(p string) cache.Entry { return cache.Entry{Path: p, Type: scan.TypePhoto} }

	for _, tc := range []struct {
		name    string
		entries []cache.Entry
		want    [][]string
	}{
		{"none", nil, nil},
		{"one video", []cache.Entry{vid("/a.mp4")}, [][]string{{"mpv", "--loop", "/a.mp4"}}},
		{"videos", []cache.Entry{vid("/a.mp4"), vid("/b.mov")},
			[][]string{{"mpv", "--loop-playlist=inf", "/a.mp4", "/b.mov"}}},
		{"one photo", []cache.Entry{photo("/p.jpg")}, [][]string{{"xdg-open", "/p.jpg"}}},
		{"mixed", []cache.Entry{photo("/p.jpg"), vid("/a.mp4")},
			[][]string{{"xdg-open", "/p.jpg"}, {"xdg-open", "/a.mp4"}}},
	} {
		var got [][]string
		for _, cmd := range externalOpenCmds(tc.entries) {
			got = append(got, cmd.Args)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestExternalOpenCmdsCapsXdgOpen: a large mixed selection launches at most
// maxExternalOpens xdg-open processes (round-4 openExternally decision); an
// all-video selection stays one mpv playlist with every file.
func TestExternalOpenCmdsCapsXdgOpen(t *testing.T) {
	var photos, videos []cache.Entry
	for i := range maxExternalOpens + 5 {
		photos = append(photos, cache.Entry{Path: fmt.Sprintf("/lib/p%d.jpg", i), Type: scan.TypePhoto})
		videos = append(videos, cache.Entry{Path: fmt.Sprintf("/lib/v%d.mp4", i), Type: scan.TypeVideo})
	}
	if got := len(externalOpenCmds(photos)); got != maxExternalOpens {
		t.Errorf("xdg-open commands = %d, want %d", got, maxExternalOpens)
	}
	cmds := externalOpenCmds(videos)
	if len(cmds) != 1 || len(cmds[0].Args) != 2+len(videos) {
		t.Errorf("all-video selection: %d commands, args %v; want one mpv with every file", len(cmds), cmds[0].Args)
	}
}
