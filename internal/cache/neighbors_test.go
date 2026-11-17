package cache

// Neighbors is the self-counting form of NeighborsWithTotal. Only the tests use
// it (production callers hold the view total already), so it lives here as a
// test helper rather than in the package API (C4-05).
func (i *Index) Neighbors(v View, path string) (prev, next *Entry, pos, total int) {
	return i.NeighborsWithTotal(v, path, i.CountView(v))
}
