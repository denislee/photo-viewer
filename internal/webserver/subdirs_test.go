package webserver

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dns/photo-viewer/internal/scan"
)

// renderSubdirsGroupedRef is the pre-W4-09 implementation kept verbatim as an
// oracle: map-of-year buckets, an explicit year sort, a per-bucket dir sort and
// FindStringSubmatch year extraction. The single-pass rewrite in sidebar.go must
// produce byte-identical output for sorted input.
func (s *Server) renderSubdirsGroupedRef(w http.ResponseWriter, subdirs []string, counts map[string]int, vi viewInfo) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	type bucket struct {
		dirs  []string
		total int
	}
	buckets := map[string]*bucket{}
	var nonDate []string
	for _, d := range subdirs {
		m := scan.DateFolderRe.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			nonDate = append(nonDate, d)
			continue
		}
		b, ok := buckets[m[1]]
		if !ok {
			b = &bucket{}
			buckets[m[1]] = b
		}
		b.dirs = append(b.dirs, d)
		b.total += counts[d]
	}

	for _, d := range nonDate {
		href := withExtra(s.dirHref(d))
		active := vi.kind == "dir" && vi.v.Dir == d
		sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
	}

	years := make([]string, 0, len(buckets))
	for y := range buckets {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		b := buckets[y]
		// Open the bucket by default if it contains the active directory,
		// so users land on an expanded tree without an extra click.
		openAttr := ""
		for _, d := range b.dirs {
			if vi.kind == "dir" && vi.v.Dir == d {
				openAttr = " open"
				break
			}
		}
		fmt.Fprintf(w,
			`<details class="year-bucket"%s><summary><span class="label">%s</span><span class="badge-count">%d</span></summary>`,
			openAttr, html.EscapeString(y), b.total)
		sort.Strings(b.dirs)
		for _, d := range b.dirs {
			href := withExtra(s.dirHref(d))
			active := vi.kind == "dir" && vi.v.Dir == d
			sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
		}
		fmt.Fprint(w, `</details>`)
	}
}

// TestRenderSubdirsGroupedMatchesReference checks the single-pass grouping
// against the reference implementation, including the open-bucket highlighting
// for the active directory and the empty-input case.
func TestRenderSubdirsGroupedMatchesReference(t *testing.T) {
	s, _, cleanup := fixture(t, 0)
	defer cleanup()

	root := s.libraryRoot
	// Already in os.ReadDir (lexical) order; sorted again for safety.
	names := []string{
		"2023-01-05",
		"2023-05",
		"2023-12-31",
		"2023-foo",
		"2024-02-01",
		"2024-02-02",
		"Albums",
		"misc",
	}
	subdirs := make([]string, 0, len(names))
	counts := make(map[string]int, len(names))
	for i, name := range names {
		p := filepath.Join(root, name)
		subdirs = append(subdirs, p)
		counts[p] = (i + 1) * 7
	}
	sort.Strings(subdirs)

	cases := []struct {
		name string
		vi   viewInfo
	}{
		{"all", s.newViewInfo("all", 0, "", "All", true)},
		{"dir-2023-12-31", s.newViewInfo("dir", 0, filepath.Join(root, "2023-12-31"), "Photos", false)},
		{"dir-Albums", s.newViewInfo("dir", 0, filepath.Join(root, "Albums"), "All", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderToString(func(w http.ResponseWriter) {
				s.renderSubdirsGrouped(w, subdirs, counts, tc.vi)
			})
			want := renderToString(func(w http.ResponseWriter) {
				s.renderSubdirsGroupedRef(w, subdirs, counts, tc.vi)
			})
			if got != want {
				t.Fatalf("output differs from reference\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}

			if tc.name == "dir-2023-12-31" {
				if !strings.Contains(got, `class="year-bucket" open`) {
					t.Errorf("expected open 2023 bucket, got:\n%s", got)
				}
				if n := strings.Count(got, " open>"); n != 1 {
					t.Errorf("expected exactly one open bucket, got %d:\n%s", n, got)
				}
			}
		})
	}

	t.Run("nil-subdirs", func(t *testing.T) {
		vi := s.newViewInfo("all", 0, "", "All", true)
		got := renderToString(func(w http.ResponseWriter) {
			s.renderSubdirsGrouped(w, nil, nil, vi)
		})
		want := renderToString(func(w http.ResponseWriter) {
			s.renderSubdirsGroupedRef(w, nil, nil, vi)
		})
		if got != want {
			t.Fatalf("output differs from reference\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
		if got != "" {
			t.Errorf("expected empty output for nil subdirs, got %q", got)
		}
	})
}

// renderToString runs fn against a recorder and returns the body as a string.
func renderToString(fn func(http.ResponseWriter)) string {
	rec := httptest.NewRecorder()
	fn(rec)
	return rec.Body.String()
}
