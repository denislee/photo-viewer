package cache

import "testing"

// TestPersonAndGroupViews guards the W11 virtual views: "person" lists every
// photo with a face of that person (once, even with two faces in it), "group"
// the photos of an unnamed cluster, both paged by path with the type filter.
func TestPersonAndGroupViews(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 4)
	a := mustCreatePerson(t, idx, "A")
	f0, c0 := writeOneFace(t, idx, "/lib/000000.jpg", vec(0, 1), [4]float64{0.1, 0.1, 0.1, 0.1})
	_ = f0
	// Same cluster, two more photos; one photo with two of the faces.
	for _, p := range []string{"/lib/000002.jpg", "/lib/000003.jpg"} {
		if _, err := idx.WriteFacesForPath(p, 1, []FaceOp{
			{Face: Face{Path: p, Embedding: vec(0, 1), NBox: [4]float64{0.1, 0.1, 0.1, 0.1}}, ExistingClusterID: c0, UpdatedCentroid: vec(0, 1)},
			{Face: Face{Path: p, Embedding: vec(0, 1), NBox: [4]float64{0.5, 0.5, 0.1, 0.1}}, ExistingClusterID: c0, UpdatedCentroid: vec(0, 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	group := View{Kind: "group", ClusterID: c0, Filter: "All", ShowRAW: true}
	if n := idx.CountView(group); n != 3 {
		t.Fatalf("group view count = %d, want 3", n)
	}
	if _, err := idx.AssignClusterToPerson(c0, a); err != nil {
		t.Fatal(err)
	}
	if n := idx.CountView(group); n != 0 {
		t.Errorf("named cluster still listed as unnamed group: %d", n)
	}
	v := View{Kind: "person", PersonID: a, Filter: "All", ShowRAW: true}
	got := idx.ListPage(v, 0, 0)
	if len(got) != 3 || got[0].Path != "/lib/000000.jpg" || got[2].Path != "/lib/000003.jpg" {
		t.Fatalf("person view = %v, want the 3 photos in path order", got)
	}
	if page := idx.ListPageAfter(v, "/lib/000000.jpg", 1); len(page) != 1 || page[0].Path != "/lib/000002.jpg" {
		t.Errorf("keyset page after first = %v", page)
	}
	if n := idx.CountView(View{Kind: "person", PersonID: a, Filter: "Videos", ShowRAW: true}); n != 0 {
		t.Errorf("Videos filter on a photo-only person = %d, want 0", n)
	}
}
