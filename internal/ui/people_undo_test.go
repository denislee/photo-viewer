package ui

import (
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestPeopleUndoStack guards W15: every people op pushes its inverse, the
// stack is capped at maxUndo, Ctrl+Z (UndoPeople) restores the previous
// state and shows an "Undone: …" toast, and an empty stack says so.
func TestPeopleUndoStack(t *testing.T) {
	c := newTestController(t, t.TempDir(), t.TempDir())
	id, err := c.Index().CreatePerson("Ana")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	op := func(hidden bool) {
		c.PeopleOp(func(idx *cache.Index) (*cache.Undo, string, error) {
			u, err := idx.HidePerson(id, hidden)
			return u, "toggled", err
		}, func(err error) { done <- err })
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("PeopleOp did not finish")
		}
	}
	for k := range maxUndo + 5 {
		op(k%2 == 0) // ends hidden=false after an even count... last k = 54 → hidden
	}
	c.faces.mu.Lock()
	n := len(c.faces.undo)
	c.faces.mu.Unlock()
	if n != maxUndo {
		t.Fatalf("undo stack = %d, want capped at %d", n, maxUndo)
	}
	hidden := func() bool {
		ps, _ := c.Index().ListPeople()
		return ps[0].Hidden
	}
	if !hidden() {
		t.Fatal("setup: last op should have hidden the person")
	}
	c.UndoPeople()
	deadline := time.Now().Add(5 * time.Second)
	for hidden() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hidden() {
		t.Fatal("UndoPeople did not restore the previous state")
	}
	for time.Now().Before(deadline) {
		if msg, ok := c.Toast(); ok && msg == "Undone: Hide person" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if msg, _ := c.Toast(); msg != "Undone: Hide person" {
		t.Errorf("toast = %q, want %q", msg, "Undone: Hide person")
	}
	c.faces.mu.Lock()
	c.faces.undo = nil
	c.faces.mu.Unlock()
	c.UndoPeople()
	if msg, _ := c.Toast(); msg != "Nothing to undo" {
		t.Errorf("empty-stack toast = %q", msg)
	}
}
