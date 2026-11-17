//go:build race

package webserver

// raceEnabled reports whether the test binary was built with -race. sync.Pool
// deliberately drops a share of Puts under the race detector, so allocation
// guards that rely on pooling skip themselves there.
const raceEnabled = true
