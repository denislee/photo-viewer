package ui

import "os"

// debugTiming enables timing logs for perceived-latency checks: the viewer's
// time to full resolution after Show/Next/Prev, and the grid's time to fill
// the viewport after a jump. Set PV_DEBUG_TIMING=1 to turn it on.
var debugTiming = os.Getenv("PV_DEBUG_TIMING") != ""
