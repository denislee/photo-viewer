package webserver

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestNewViewInfoMatchesViewFromQuery pins the W4-03 invariant: the gallery
// handlers' constructor and viewFromQuery must produce identical viewInfo for
// the same request, so a page and the viewer/api context it forwards can't
// disagree about the title, back link or forwarded query.
func TestNewViewInfoMatchesViewFromQuery(t *testing.T) {
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	sub := filepath.Join(s.libraryRoot, "2024-05-01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		query        url.Values
		kind         string
		year         int
		dirAbs       string
		wantTitle    string
		wantBackHref string
		wantCtxQuery string
	}{
		{
			name:         "all default",
			query:        url.Values{"from": {"all"}},
			kind:         "all",
			wantTitle:    "All media",
			wantBackHref: "/",
			wantCtxQuery: "from=all",
		},
		{
			name:         "all filtered",
			query:        url.Values{"from": {"all"}, "filter": {"Photos"}, "raw": {"0"}},
			kind:         "all",
			wantTitle:    "All media",
			wantBackHref: "/?filter=Photos&raw=0",
			wantCtxQuery: "from=all&filter=Photos&raw=0",
		},
		{
			name:         "favorites",
			query:        url.Values{"from": {"favorites"}, "raw": {"0"}},
			kind:         "favorites",
			wantTitle:    "Favorites",
			wantBackHref: "/favorites?raw=0",
			wantCtxQuery: "from=favorites&raw=0",
		},
		{
			name:         "trash keeps bare back link",
			query:        url.Values{"from": {"trash"}, "filter": {"Videos"}},
			kind:         "trash",
			wantTitle:    "Trash",
			wantBackHref: "/trash",
			wantCtxQuery: "from=trash&filter=Videos",
		},
		{
			name:         "year canonical label",
			query:        url.Values{"from": {"year"}, "y": {"02024"}},
			kind:         "year",
			year:         2024,
			wantTitle:    "Year 2024",
			wantBackHref: "/year/2024",
			wantCtxQuery: "from=year&y=2024",
		},
		{
			name:         "dir",
			query:        url.Values{"from": {"dir"}, "path": {"2024-05-01"}},
			kind:         "dir",
			dirAbs:       sub,
			wantTitle:    "2024-05-01",
			wantBackHref: "/dir?path=2024-05-01",
			wantCtxQuery: "from=dir&path=2024-05-01",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.viewFromQuery(tc.query)
			if !ok {
				t.Fatalf("viewFromQuery(%v) = _, false; want true", tc.query)
			}
			want := s.newViewInfo(tc.kind, tc.year, tc.dirAbs,
				parseFilter(tc.query.Get("filter")), parseShowRAW(tc.query))
			if got != want {
				t.Fatalf("viewFromQuery(%v) =\n  %+v\nwant\n  %+v", tc.query, got, want)
			}
			if got.title != tc.wantTitle {
				t.Errorf("title = %q; want %q", got.title, tc.wantTitle)
			}
			if got.backHref != tc.wantBackHref {
				t.Errorf("backHref = %q; want %q", got.backHref, tc.wantBackHref)
			}
			if got.ctxQuery != tc.wantCtxQuery {
				t.Errorf("ctxQuery = %q; want %q", got.ctxQuery, tc.wantCtxQuery)
			}
		})
	}

	// Views that can't be reconstructed must be rejected instead of silently
	// falling back to the index view.
	for _, q := range []url.Values{
		{"from": {"year"}, "y": {"abc"}},
		{"from": {"dir"}, "path": {"../x"}},
		{"from": {"bogus"}},
	} {
		if _, ok := s.viewFromQuery(q); ok {
			t.Errorf("viewFromQuery(%v) = _, true; want false", q)
		}
	}
}
