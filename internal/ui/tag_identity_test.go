package ui

import "testing"

// rowTagsEscaped allocates tags the way Sidebar.Layout and the search view do
// (heap-allocated, stored in a slice that outlives the call).
//
//go:noinline
func rowTagsEscaped() []any {
	return []any{&sidebarTag{}, &sidebarTag{}, &searchRowTag{}, &searchRowTag{}}
}

// TestRowTagsAreDistinct guards G6-14: the sidebar and search row tags are
// identity-only, and Gio routes pointer events by tag pointer. A zero-size tag
// type lets the runtime hand every heap-allocated row the same address
// (runtime.zerobase), so all rows would answer one row's clicks.
func TestRowTagsAreDistinct(t *testing.T) {
	tags := rowTagsEscaped()
	if tags[0] == tags[1] {
		t.Error("two sidebarTag allocations share an address")
	}
	if tags[2] == tags[3] {
		t.Error("two searchRowTag allocations share an address")
	}
}
