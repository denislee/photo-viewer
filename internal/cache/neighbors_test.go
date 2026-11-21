package cache

// Neighbors returns the same prev/next/pos as NeighborsWithTotal plus the view
// total. Only the tests use it (production callers hold the view total
// already), so it lives here as a test helper rather than in the package API
// (C4-05).
func (i *Index) Neighbors(v View, path string) (prev, next *Entry, pos, total int) {
	prev, next, pos = i.NeighborsWithTotal(v, path)
	return prev, next, pos, i.CountView(v)
}
