package ui

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// debugTiming enables timing logs for perceived-latency checks: the viewer's
// time to full resolution after Show/Next/Prev, and the grid's time to fill
// the viewport after a jump. Set PV_DEBUG_TIMING=1 to turn it on.
var debugTiming = os.Getenv("PV_DEBUG_TIMING") != ""

// debugStartView opens a People view at startup when PV_DEBUG_START_VIEW is
// set, so the People UI can be driven and screenshotted headlessly (no seat
// keyboard needed): "settings", "people", "person:<id>" or "group:<id>", optionally with
// "+review" to open the Review faces overlay on that person/group. The
// library scan is let finish first (SelectDir would cancel it).
func debugStartView(ctrl *Controller, views *appViews, invalidate interface{ Invalidate() }) {
	spec := os.Getenv("PV_DEBUG_START_VIEW")
	if spec == "" {
		return
	}
	review := strings.HasSuffix(spec, "+review")
	spec = strings.TrimSuffix(spec, "+review")
	var dir string
	var target ReviewTarget
	switch {
	case spec == "settings":
		go func() {
			time.Sleep(3 * time.Second)
			views.settings.Show()
			invalidate.Invalidate()
		}()
		return
	case spec == "people":
		dir = PeopleView
	case strings.HasPrefix(spec, "person:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(spec, "person:"), 10, 64)
		dir, target = PersonDir(id), ReviewTarget{PersonID: id, Title: "Person"}
	case strings.HasPrefix(spec, "group:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(spec, "group:"), 10, 64)
		dir, target = GroupDir(id), ReviewTarget{ClusterID: id, Title: "Unnamed group"}
	default:
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		ctrl.SelectDir(dir)
		if review && (target.PersonID > 0 || target.ClusterID > 0) {
			views.review.Show(target)
		}
		invalidate.Invalidate()
	}()
}
