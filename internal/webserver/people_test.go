package webserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// peopleState is what the People tests need to build URLs and assertions from
// the fixture: the created person and the face confirmed as them.
type peopleState struct {
	personID int64
	faceID   int64
	thumbID  string
}

// peopleFixture builds a server over a one-photo index holding one confirmed
// face ("Alice") on a real JPEG, so the thumbnail and the face crop /face
// serves are generated through the production code path rather than stubbed.
// A second person ("Bob") is created and hidden, so the visible-only filtering
// of /people and CountPeople is exercised too.
func peopleFixture(t *testing.T) (*Server, *httptest.Server, peopleState, func()) {
	t.Helper()
	tmp := t.TempDir()
	libRoot := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		idx.Close()
		t.Fatal(err)
	}

	jpegBytes := makeDisplayJPEG(t, 64, 48)
	photo := filepath.Join(libRoot, "IMG_0001.jpg")
	if err := os.WriteFile(photo, jpegBytes, 0o644); err != nil {
		idx.Close()
		t.Fatal(err)
	}
	mtime := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(photo, mtime, mtime); err != nil {
		idx.Close()
		t.Fatal(err)
	}
	idx.ReconcileBatch([]scan.Result{{
		Path:    photo,
		Type:    scan.TypePhoto,
		Size:    int64(len(jpegBytes)),
		ModTime: mtime,
	}})

	s := New(idx, store, nil, libRoot, filepath.Join(libRoot, ".photo-viewer-trash"))
	s.SetFaceCacheDir(tmp)

	personID, err := idx.CreatePerson("Alice")
	if err != nil {
		idx.Close()
		t.Fatal(err)
	}
	hiddenID, err := idx.CreatePerson("Bob")
	if err != nil {
		idx.Close()
		t.Fatal(err)
	}
	if _, err := idx.HidePerson(hiddenID, true); err != nil {
		idx.Close()
		t.Fatal(err)
	}

	// The bbox is in the 64x48 thumbnail's pixel space (well inside it), with
	// the matching normalised box recorded so /api/info can forward it.
	ops := []cache.FaceOp{{
		Face: cache.Face{
			Path:      photo,
			BBox:      [4]int{8, 8, 32, 32},
			NBox:      [4]float64{0.125, 0.16667, 0.5, 0.66667},
			Embedding: []float32{0.1, 0.2},
		},
		NewClusterCentroid: []float32{0.1, 0.2},
		NewClusterPersonID: personID,
	}}
	results, err := idx.WriteFacesForPath(photo, mtime.Unix(), ops)
	if err != nil {
		idx.Close()
		t.Fatalf("WriteFacesForPath: %v", err)
	}
	if len(results) != 1 {
		idx.Close()
		t.Fatalf("WriteFacesForPath wrote %d faces, want 1", len(results))
	}
	faceID := results[0].FaceID
	// Confirm it, which is what puts the person's name on the face row and
	// marks it assign 'user' (the "confirmed" flag in /api/info).
	if _, err := idx.TagFaces([]int64{faceID}, cache.VerdictConfirmed, personID); err != nil {
		idx.Close()
		t.Fatalf("TagFaces: %v", err)
	}

	ts := httptest.NewServer(s.handler(""))
	return s, ts, peopleState{
		personID: personID,
		faceID:   faceID,
		thumbID:  cache.ThumbIDFor(photo),
	}, func() {
		ts.Close()
		idx.Close()
	}
}

// getBody fetches rawurl and returns the status and body.
func getBody(t *testing.T, rawurl string) (int, string) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestPeoplePageListsVisiblePerson(t *testing.T) {
	_, ts, st, cleanup := peopleFixture(t)
	defer cleanup()

	code, body := getBody(t, ts.URL+"/people")
	if code != http.StatusOK {
		t.Fatalf("GET /people = %d, want 200", code)
	}
	pid := strconv.FormatInt(st.personID, 10)
	for _, want := range []string{
		`<h1>People <span class="count">1 people</span></h1>`,
		`href="/person/` + pid + `"`,
		`src="/face/` + strconv.FormatInt(st.faceID, 10) + `"`,
		"Alice · 1 photos",
		// The gallery chrome is reused: hamburger + layout + sidebar + mobile bar.
		`id="navtoggle"`,
		`<div class="layout">`,
		`class="sidebar"`,
		`class="topbar"`,
		// The People row is marked active on its own page.
		`class="row active" href="/people"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/people missing %q", want)
		}
	}
	// Hidden people are not listed.
	if strings.Contains(body, "Bob") {
		t.Errorf("/people lists the hidden person Bob")
	}
}

func TestPersonGalleryRendersItsPhotos(t *testing.T) {
	_, ts, st, cleanup := peopleFixture(t)
	defer cleanup()

	pid := strconv.FormatInt(st.personID, 10)
	code, body := getBody(t, ts.URL+"/person/"+pid)
	if code != http.StatusOK {
		t.Fatalf("GET /person/%s = %d, want 200", pid, code)
	}
	if want := `<h1>Alice <span class="count">1 items</span></h1>`; !strings.Contains(body, want) {
		t.Errorf("/person/%s header missing %q", pid, want)
	}
	// The cell keeps the person context so the viewer and /api/page stay inside
	// the person's photo list.
	wantView := "/view/" + st.thumbID + "?from=person&pid=" + pid
	if !strings.Contains(body, wantView) {
		t.Errorf("/person/%s grid missing cell link %q", pid, wantView)
	}
	// The toolbar is built from the person's base path.
	if want := `href="/person/` + pid + `?filter=Photos"`; !strings.Contains(body, want) {
		t.Errorf("/person/%s toolbar missing %q", pid, want)
	}
}

func TestPersonAndFaceUnknownIDsAre404(t *testing.T) {
	_, ts, _, cleanup := peopleFixture(t)
	defer cleanup()

	for _, path := range []string{
		"/person/999",
		"/person/0",
		"/person/-1",
		"/person/abc",
		"/face/0",
		"/face/-3",
		"/face/999999",
		"/face/abc",
	} {
		if code, _ := getBody(t, ts.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// TestFaceRouteServesCachedCrop covers the /face path end to end: the face row
// resolves to its photo, the photo to its cached thumbnail, and the crop is
// written to faceCacheDir and served as a JPEG with a private day cache.
func TestFaceRouteServesCachedCrop(t *testing.T) {
	_, ts, st, cleanup := peopleFixture(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/face/" + strconv.FormatInt(st.faceID, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /face/%d = %d, want 200", st.faceID, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != faceCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cc, faceCacheControl)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte{0xFF, 0xD8, 0xFF}) {
		t.Errorf("crop is not a JPEG (first bytes %x)", body[:min(len(body), 3)])
	}
}

// TestAPIInfoIncludesFaces pins the /api/info face payload: the confirmed face
// carries its normalised box, the person's name and id, and the confirmed flag,
// while the existing metadata fields are unchanged. Faces must come from the
// index (not the file-version metadata cache), so a rename shows up at once —
// this test renames the person and re-requests the same unchanged file.
func TestAPIInfoIncludesFaces(t *testing.T) {
	s, ts, st, cleanup := peopleFixture(t)
	defer cleanup()

	type face struct {
		Box       []float64 `json:"box"`
		Name      string    `json:"name"`
		Person    int64     `json:"person"`
		Confirmed bool      `json:"confirmed"`
	}
	type info struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Faces []face `json:"faces"`
	}
	fetch := func() info {
		t.Helper()
		code, body := getBody(t, ts.URL+"/api/info?id="+st.thumbID)
		if code != http.StatusOK {
			t.Fatalf("GET /api/info = %d, want 200", code)
		}
		var got info
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode /api/info: %v\n%s", err, body)
		}
		return got
	}

	got := fetch()
	if got.ID != st.thumbID || got.Name != "IMG_0001.jpg" {
		t.Errorf("info identity = %q/%q, want %q/IMG_0001.jpg", got.ID, got.Name, st.thumbID)
	}
	if len(got.Faces) != 1 {
		t.Fatalf("faces = %d, want 1 (%+v)", len(got.Faces), got)
	}
	f := got.Faces[0]
	if f.Name != "Alice" {
		t.Errorf("face name = %q, want Alice", f.Name)
	}
	if f.Person != st.personID {
		t.Errorf("face person = %d, want %d", f.Person, st.personID)
	}
	if !f.Confirmed {
		t.Errorf("face confirmed = false, want true")
	}
	if len(f.Box) != 4 || f.Box[2] != 0.5 || f.Box[3] != 0.66667 {
		t.Errorf("face box = %v, want [0.125 0.16667 0.5 0.66667]", f.Box)
	}

	// Same file version, new name: the faces must not be stale.
	if _, err := s.index.RenamePerson(st.personID, "Alicia"); err != nil {
		t.Fatalf("RenamePerson: %v", err)
	}
	if renamed := fetch(); len(renamed.Faces) != 1 || renamed.Faces[0].Name != "Alicia" {
		t.Errorf("faces after rename = %+v, want Alicia", renamed.Faces)
	}
}

func TestViewFromQueryPersonRoundTrip(t *testing.T) {
	s, _, st, cleanup := peopleFixture(t)
	defer cleanup()

	pid := strconv.FormatInt(st.personID, 10)
	vi, ok := s.viewFromQuery(url.Values{"from": {"person"}, "pid": {pid}})
	if !ok {
		t.Fatalf("viewFromQuery(from=person&pid=%s) = _, false", pid)
	}
	if vi.kind != "person" || vi.v.Kind != "person" || vi.v.PersonID != st.personID {
		t.Errorf("view = kind %q / v.Kind %q / pid %d, want person/person/%d",
			vi.kind, vi.v.Kind, vi.v.PersonID, st.personID)
	}
	if vi.title != "Alice" {
		t.Errorf("title = %q, want Alice", vi.title)
	}
	if want := "/person/" + pid; vi.base != want || vi.backHref != want {
		t.Errorf("base/backHref = %q/%q, want %q", vi.base, vi.backHref, want)
	}
	if want := "from=person&pid=" + pid; vi.ctxQuery != want {
		t.Errorf("ctxQuery = %q, want %q", vi.ctxQuery, want)
	}

	// The toolbar toggles thread through the reconstructed view.
	vi, ok = s.viewFromQuery(url.Values{
		"from": {"person"}, "pid": {pid}, "filter": {"Photos"}, "raw": {"0"},
	})
	if !ok {
		t.Fatalf("viewFromQuery with toggles = _, false")
	}
	if want := "from=person&filter=Photos&raw=0&pid=" + pid; vi.ctxQuery != want {
		t.Errorf("ctxQuery = %q, want %q", vi.ctxQuery, want)
	}
	if want := "/person/" + pid + "?filter=Photos&raw=0"; vi.backHref != want {
		t.Errorf("backHref = %q, want %q", vi.backHref, want)
	}

	// Missing, malformed and unknown pids can't be reconstructed.
	for _, q := range []url.Values{
		{"from": {"person"}},
		{"from": {"person"}, "pid": {"0"}},
		{"from": {"person"}, "pid": {"-2"}},
		{"from": {"person"}, "pid": {"abc"}},
		{"from": {"person"}, "pid": {"999"}},
	} {
		if _, ok := s.viewFromQuery(q); ok {
			t.Errorf("viewFromQuery(%v) = _, true; want false", q)
		}
	}
}
