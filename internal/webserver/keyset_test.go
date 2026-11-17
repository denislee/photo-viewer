package webserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// keysetPage is the subset of /api/page's JSON payload these tests read.
type keysetPage struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	HasNext bool `json:"hasNext"`
}

// ids projects the page's item ids, in order.
func (p keysetPage) ids() []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.ID
	}
	return out
}

// TestAPIPageKeysetMatchesOffset proves W4-05's keyset path returns exactly the
// same items as the OFFSET path, page for page, and that an unusable cursor
// falls back to the plain offset page.
func TestAPIPageKeysetMatchesOffset(t *testing.T) {
	_, ts, cleanup := fixture(t, 450)
	defer cleanup()

	fetch := func(p int, after string) keysetPage {
		t.Helper()
		u := fmt.Sprintf("%s/api/page?from=all&p=%d", ts.URL, p)
		if after != "" {
			u += "&after=" + url.QueryEscape(after)
		}
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", u, resp.StatusCode)
		}
		var pg keysetPage
		if err := json.NewDecoder(resp.Body).Decode(&pg); err != nil {
			t.Fatalf("GET %s: decode: %v", u, err)
		}
		return pg
	}

	// First page: the keyset cursor comes from its last id.
	first := fetch(1, "")
	if len(first.Items) != pageSize {
		t.Fatalf("page 1 items = %d, want %d", len(first.Items), pageSize)
	}

	// Offset walk from page 2 on.
	var offsetIDs []string
	for p := 2; p < 100; p++ {
		pg := fetch(p, "")
		offsetIDs = append(offsetIDs, pg.ids()...)
		if !pg.HasNext {
			break
		}
	}

	// Keyset walk from page 2 on, cursoring on the last id seen so far.
	var keysetIDs []string
	last := first.Items[len(first.Items)-1].ID
	for p := 2; p < 100; p++ {
		pg := fetch(p, last)
		keysetIDs = append(keysetIDs, pg.ids()...)
		if !pg.HasNext {
			break
		}
		if len(pg.Items) == 0 {
			t.Fatalf("page %d: hasNext=true with no items", p)
		}
		last = pg.Items[len(pg.Items)-1].ID
	}

	if !slices.Equal(keysetIDs, offsetIDs) {
		n := -1
		for i := 0; i < len(keysetIDs) && i < len(offsetIDs); i++ {
			if keysetIDs[i] != offsetIDs[i] {
				n = i
				break
			}
		}
		t.Errorf("keyset pages differ from offset pages: keyset=%d ids, offset=%d ids, first diff at %d",
			len(keysetIDs), len(offsetIDs), n)
	}
	if got, want := len(first.Items)+len(keysetIDs), 450; got != want {
		t.Errorf("keyset walk returned %d ids total, want %d", got, want)
	}
	if got, want := len(first.Items)+len(offsetIDs), 450; got != want {
		t.Errorf("offset walk returned %d ids total, want %d", got, want)
	}

	// An unknown-but-well-formed cursor must fall back to the offset page.
	unknown := strings.Repeat("a", 40)
	fallback := fetch(2, unknown)
	plain := fetch(2, "")
	if !slices.Equal(fallback.ids(), plain.ids()) {
		t.Errorf("after=<unknown id> page 2 = %d ids, want the %d ids of the plain p=2 page",
			len(fallback.Items), len(plain.Items))
	}
	if fallback.HasNext != plain.HasNext {
		t.Errorf("after=<unknown id> hasNext = %v, want %v (offset fallback)", fallback.HasNext, plain.HasNext)
	}
}
