package ui

import "testing"

// TestProcessBarPruneAfterLastProcess covers G4-13: once the last process
// ends, the row-button map must empty even though Layout no longer runs.
func TestProcessBarPruneAfterLastProcess(t *testing.T) {
	pb := NewProcessBar(nil)
	pb.row[1] = &processRowButtons{}
	pb.row[2] = &processRowButtons{}

	pb.Prune([]ProcessSnapshot{{ID: 2}})
	if _, ok := pb.row[1]; ok || len(pb.row) != 1 {
		t.Fatalf("after process 1 ended: rows %v, want only 2", pb.row)
	}
	pb.Prune(nil)
	if len(pb.row) != 0 {
		t.Fatalf("after the last process ended: %d rows left, want 0", len(pb.row))
	}
}
