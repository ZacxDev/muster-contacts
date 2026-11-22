package store

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// rawFile is the database file and its WAL, as bytes on disk.
func rawFile(s *Store) []byte {
	var all []byte
	for _, p := range []string{s.Path(), s.Path() + "-wal"} {
		b, _ := os.ReadFile(p)
		all = append(all, b...)
	}
	return all
}

// G1: the checkpoint runs whatever its caller's context says: a request
// whose client went away after the delete committed still clears the WAL.
func TestTheCheckpointIgnoresACancelledContext(t *testing.T) {
	s := open(t)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.checkpoint(gone); err != nil {
		t.Fatalf("checkpoint on a cancelled context: %v, want it to run", err)
	}
}

// G3: an email removed by Edit, and a note on a contact merged away, leave
// the file too (the same checkpoint as a delete), with the control that
// each is in the file before.
func TestAnEditAndAMergeLeaveNoTraceInTheFile(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	const dropped, note = "dropped.address.fixture@example.test", "a note-fixture-that-goes-with-the-merge"
	c, err := o.Create(ctx, ContactIn{Name: "Kept", Emails: []string{"kept@example.test", dropped}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	from, err := o.Create(ctx, ContactIn{Name: "Other", Note: note}, t0)
	if err != nil {
		t.Fatal(err)
	}
	s.checkpoint(ctx)
	for _, f := range []string{dropped, note} {
		if !bytes.Contains(rawFile(s), []byte(f)) {
			t.Fatalf("before: the file does not hold %q: the read cannot see it", f)
		}
	}
	if _, err := o.Update(ctx, c.ID, ContactPatch{Emails: &[]string{"kept@example.test"}}, t0); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawFile(s), []byte(dropped)) {
		t.Errorf("after an Edit removed it, the file still holds %q", dropped)
	}
	if _, err := o.Merge(ctx, c.ID, from.ID, t0); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawFile(s), []byte(note)) {
		t.Errorf("after a merge deleted its contact, the file still holds %q", note)
	}
}
