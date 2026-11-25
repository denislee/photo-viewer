package ui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// faceTestWait bounds every poll below. Detection forks one Python daemon per
// worker (~50ms each) and a Rebuild re-detects + regroups the whole library, so
// 20s is a generous ceiling that still fails fast when something is wedged.
const faceTestWait = 20 * time.Second

// fakeFaceHelperScript is a stand-in for pv-face-detect: a Python script whose
// --check reports the interpreter and whose --server answers one NDJSON request
// per line with a single synthetic face.
//
// The pipeline hands the helper the *thumbnail* path, not the photo path, and
// thumbnails are named by sha1 (cache.ThumbIDFor), so the photo's name is not
// visible to the helper. The test therefore seeds a thumbnail → group map next
// to the script (seedFaceGroups) and the helper falls back to the "ana" rule on
// the path it was given. Group "ana" gets embedding 0, everything else
// embedding 1 — a squared distance of 2 between groups, far past
// face.MaxClusterSqDistance, so they land in two clusters.
const fakeFaceHelperScript = `#!/usr/bin/env python3
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
MAP_PATH = os.path.join(HERE, "face-groups.json")


def groups():
    try:
        with open(MAP_PATH) as f:
            return json.load(f)
    except Exception:
        return {}


def group_of(path):
    g = groups().get(path)
    if g is None:
        g = "ana" if "ana" in os.path.basename(path) else "other"
    return 0 if g == "ana" else 1


def embedding(path):
    e = [0.0] * 128
    e[group_of(path)] = 1.0
    return e


def main():
    args = sys.argv[1:]
    if "--check" in args:
        print("ok " + sys.executable, flush=True)
        return 0
    if "--server" in args:
        while True:
            line = sys.stdin.readline()
            if not line:
                break
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except Exception:
                continue
            resp = {
                "detections": [{
                    "bbox": [10, 10, 40, 40],
                    "embedding": embedding(req.get("path", "")),
                    "quality": 0.9,
                }],
                "size": [100, 100],
            }
            print(json.dumps(resp), flush=True)
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
`

// installFakeFaceHelper puts an executable fake pv-face-detect at the head of
// PATH and re-probes the helper so the pipeline sees it. It returns the
// directory holding the script (where seedFaceGroups drops its map file).
//
// The cleanup that re-probes is registered *before* t.Setenv so that, cleanups
// being LIFO, it runs after PATH is restored: later tests then see the real
// machine state instead of a PATH pointing at a deleted temp dir.
func installFakeFaceHelper(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; the fake face helper is a Python script")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, face.BinaryName)
	if err := os.WriteFile(bin, []byte(fakeFaceHelperScript), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}

	t.Cleanup(func() { face.RefreshStatus() })
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if st := face.RefreshStatus(); !st.Working {
		t.Fatalf("fake helper did not probe as working: %+v", st)
	}
	return dir
}

// seedFaceGroups writes the thumbnail → group map the fake helper reads, so
// photos named ana* form one group and every other photo another. Thumbnails
// are generated here (through the same store.Path the pipeline uses) because
// the helper keys on the exact path it is asked about.
func seedFaceGroups(t *testing.T, c *Controller, helperDir string) {
	t.Helper()
	var entries []cache.Entry
	c.Index().ForEachEntry(func(e cache.Entry) bool {
		entries = append(entries, e)
		return true
	})
	if len(entries) == 0 {
		t.Fatal("seedFaceGroups: the index has no entries")
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		thumb, err := c.Store().Path(e)
		if err != nil {
			t.Fatalf("thumbnail for %s: %v", e.Path, err)
		}
		group := "other"
		if strings.Contains(filepath.Base(e.Path), "ana") {
			group = "ana"
		}
		m[thumb] = group
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal face groups: %v", err)
	}
	if err := os.WriteFile(filepath.Join(helperDir, "face-groups.json"), data, 0o644); err != nil {
		t.Fatalf("write face groups: %v", err)
	}
}

// isolateFaceTestConfig points the config at a throwaway XDG_CONFIG_HOME, loads
// it into memory, and restores the previous in-memory config afterwards. Faces
// are on by default, so a fresh (empty) config is what these tests want.
func isolateFaceTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfgWriteMu.Lock()
	cfgMu.Lock()
	prev, prevGen, prevSaved := cfg, cfgGen, cfgSavedGen
	cfg, cfgGen, cfgSavedGen = Config{}, 0, 0
	cfgMu.Unlock()
	cfgWriteMu.Unlock()

	LoadConfig()

	t.Cleanup(func() {
		_ = FlushConfig(5 * time.Second)
		cfgWriteMu.Lock()
		cfgMu.Lock()
		cfg, cfgGen, cfgSavedGen = prev, prevGen, prevSaved
		cfgMu.Unlock()
		cfgWriteMu.Unlock()
	})
}

// waitForFaces polls cond every 20ms for up to faceTestWait. Unlike waitFor it
// allows for a Python daemon fork and a full re-detect + regroup.
func waitForFaces(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(faceTestWait)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", faceTestWait, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newFaceTestLibrary indexes a three-photo library (ana1.jpg, ana2.jpg,
// bob1.jpg) on a fresh controller with isolated config.
func newFaceTestLibrary(t *testing.T) *Controller {
	t.Helper()
	isolateFaceTestConfig(t)
	root := t.TempDir()
	for _, name := range []string{"ana1.jpg", "ana2.jpg", "bob1.jpg"} {
		writeJPEGWithOrientation(t, filepath.Join(root, name), 100, 100, 1)
	}
	c := newTestController(t, root, t.TempDir())
	c.scanInto(context.Background(), root)
	n := 0
	c.Index().ForEachEntry(func(cache.Entry) bool {
		n++
		return true
	})
	if n != 3 {
		t.Fatalf("indexed %d entries, want 3", n)
	}
	return c
}

// waitForTwoGroups waits until detection has produced exactly two unnamed
// groups and returns the cluster id of the one holding two faces.
func waitForTwoGroups(t *testing.T, c *Controller) int64 {
	t.Helper()
	var group int64
	waitForFaces(t, "two face groups", func() bool {
		groups, err := c.Index().ListGroups(1)
		if err != nil || len(groups) != 2 {
			return false
		}
		for _, g := range groups {
			if g.Faces == 2 {
				group = g.ClusterID
				return true
			}
		}
		return false
	})
	return group
}

// nameGroup runs the "type a name on a group" people op, waits for it to land
// and returns the person's id.
func nameGroup(t *testing.T, c *Controller, clusterID int64, name string) int64 {
	t.Helper()
	msg := "Named " + name
	done := make(chan error, 1)
	c.PeopleOp(func(idx *cache.Index) (*cache.Undo, string, error) {
		_, undo, err := idx.NamePersonFromCluster(clusterID, name)
		return undo, msg, err
	}, func(err error) { done <- err })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("name group %d as %q: %v", clusterID, name, err)
		}
	case <-time.After(faceTestWait):
		t.Fatalf("timed out after %s naming group %d as %q", faceTestWait, clusterID, name)
	}

	id, ok := c.Index().PersonByName(name)
	if !ok {
		t.Fatalf("PersonByName(%q) not found after naming the group", name)
	}
	return id
}

// TestFacesDetectAndNameGroup drives the whole people flow end to end: detect
// faces from the helper, cluster them into two groups, name the two-face group,
// then check the person, the toast, the person grid and the undo.
func TestFacesDetectAndNameGroup(t *testing.T) {
	c := newFaceTestLibrary(t)
	helperDir := installFakeFaceHelper(t)
	t.Cleanup(c.StopFaces)
	seedFaceGroups(t, c, helperDir)

	c.StartFaces()
	group := waitForTwoGroups(t, c)

	anaID := nameGroup(t, c, group, "Ana")

	if n := c.Index().CountPeople(); n != 1 {
		t.Fatalf("CountPeople = %d after naming one group, want 1", n)
	}
	if msg, ok := c.Toast(); !ok || msg != "Named Ana" {
		t.Fatalf("Toast() = (%q, %v), want (%q, true)", msg, ok, "Named Ana")
	}

	c.SelectDir(PersonDir(anaID))
	var got []string
	waitForFaces(t, "Ana's person grid to show her 2 photos", func() bool {
		_, dir, entries, _ := c.Snapshot()
		if dir != PersonDir(anaID) || len(entries) != 2 {
			return false
		}
		got = got[:0]
		for _, e := range entries {
			got = append(got, filepath.Base(e.Path))
		}
		return true
	})
	want := map[string]bool{"ana1.jpg": true, "ana2.jpg": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("person grid shows %q, which is not one of Ana's photos", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("person grid is missing %q", name)
	}

	if !c.CanUndoPeople() {
		t.Fatal("CanUndoPeople = false after naming a group")
	}
	c.UndoPeople()
	waitForFaces(t, "the undo to delete the person it created", func() bool {
		return c.Index().CountPeople() == 0
	})
	if _, ok := c.Index().PersonByName("Ana"); ok {
		t.Error(`PersonByName("Ana") still resolves after undoing the naming`)
	}

	c.StopFaces()
}

// TestFacesRebuildKeepsNames covers the Rebuild path: a confirmed face survives
// the index wipe as a durable tag, re-attaches to the re-detected face, and
// Regroup re-links the rest of the group to the same person.
func TestFacesRebuildKeepsNames(t *testing.T) {
	c := newFaceTestLibrary(t)
	helperDir := installFakeFaceHelper(t)
	t.Cleanup(c.StopFaces)
	seedFaceGroups(t, c, helperDir)

	c.StartFaces()
	group := waitForTwoGroups(t, c)
	anaID := nameGroup(t, c, group, "Ana")

	faces, err := c.Index().FacesOfPerson(anaID, cache.OrderPath)
	if err != nil {
		t.Fatalf("FacesOfPerson: %v", err)
	}
	if len(faces) != 2 {
		t.Fatalf("Ana has %d faces before the rebuild, want 2", len(faces))
	}
	faceID := faces[0].ID

	c.Retag([]int64{faceID}, cache.VerdictConfirmed, anaID, "ok")
	waitForFaces(t, "the confirmed face to be assigned to the user", func() bool {
		f, ok := c.Index().Face(faceID)
		return ok && f.Assign == cache.AssignUser
	})

	if err := c.Rebuild(); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	waitForFaces(t, "the rebuild to re-detect and re-group Ana's 2 faces", func() bool {
		fs, err := c.Index().FacesOfPerson(anaID, cache.OrderPath)
		return err == nil && len(fs) == 2
	})

	c.StopFaces()
}

// TestFacesDisabledSetting checks the settings toggle: with faces disabled,
// StartFaces never spins the workers up even when the helper is installed.
func TestFacesDisabledSetting(t *testing.T) {
	isolateFaceTestConfig(t)
	UpdateConfig(func(cfg *Config) { cfg.FacesDisabled = true })
	installFakeFaceHelper(t)

	root := t.TempDir()
	c := newTestController(t, root, t.TempDir())
	t.Cleanup(c.StopFaces)

	c.StartFaces()

	// Give StartFaces every chance to (wrongly) start the pipeline, then make
	// sure it never did.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.facesRunning() {
			t.Fatal("faces started while the settings toggle is off")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c.facesRunning() {
		t.Fatal("faces running 300ms after StartFaces with the toggle off")
	}
}
