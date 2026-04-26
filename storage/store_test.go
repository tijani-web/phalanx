package storage

import (
	"fmt"
	"testing"

	"github.com/basit-tijani/phalanx/pb"
)

// TestPersistence verifies that Raft state survives a full shutdown/restart
// cycle via BadgerDB. This is the foundational guarantee for crash recovery.
//
//  1. Initialize a store, save term + votedFor + 5 log entries.
//  2. Close the store (simulate process death).
//  3. Reopen the store (simulate restart).
//  4. Verify all persisted state is intact.
func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	// ---- Phase 1: Write State ----
	store, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Persist Raft hard state.
	if err := store.SaveState(5, "node-2"); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// Write 5 log entries.
	entries := make([]*pb.LogEntry, 5)
	for i := range entries {
		entries[i] = &pb.LogEntry{
			Index: uint64(i + 1),
			Term:  5,
			Type:  pb.EntryCommand,
			Data:  []byte(fmt.Sprintf("cmd-%d", i+1)),
		}
	}
	if err := store.AppendEntries(entries); err != nil {
		t.Fatalf("append entries: %v", err)
	}

	// ---- Shutdown ----
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// ---- Phase 2: Restart & Verify ----
	store2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	// Verify hard state.
	term, votedFor, err := store2.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if term != 5 {
		t.Errorf("term: want 5, got %d", term)
	}
	if votedFor != "node-2" {
		t.Errorf("votedFor: want 'node-2', got '%s'", votedFor)
	}

	// Verify log entries.
	loaded, err := store2.LoadLog()
	if err != nil {
		t.Fatalf("load log: %v", err)
	}
	if len(loaded) != 5 {
		t.Fatalf("log count: want 5, got %d", len(loaded))
	}
	for i, entry := range loaded {
		wantIdx := uint64(i + 1)
		if entry.Index != wantIdx {
			t.Errorf("entry[%d].Index: want %d, got %d", i, wantIdx, entry.Index)
		}
		if entry.Term != 5 {
			t.Errorf("entry[%d].Term: want 5, got %d", i, entry.Term)
		}
		wantData := fmt.Sprintf("cmd-%d", i+1)
		if string(entry.Data) != wantData {
			t.Errorf("entry[%d].Data: want '%s', got '%s'", i, wantData, entry.Data)
		}
	}
}

// TestTruncateFrom verifies that log truncation removes all entries
// at and after the specified index — required for conflict resolution.
func TestTruncateFrom(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Write entries 1–5.
	entries := make([]*pb.LogEntry, 5)
	for i := range entries {
		entries[i] = &pb.LogEntry{
			Index: uint64(i + 1),
			Term:  1,
			Type:  pb.EntryCommand,
			Data:  []byte(fmt.Sprintf("cmd-%d", i+1)),
		}
	}
	if err := store.AppendEntries(entries); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Truncate from index 3 → entries 3, 4, 5 gone.
	if err := store.TruncateFrom(3); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	loaded, err := store.LoadLog()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("log count after truncate: want 2, got %d", len(loaded))
	}
	if loaded[0].Index != 1 || loaded[1].Index != 2 {
		t.Errorf("remaining entries: want [1,2], got [%d,%d]",
			loaded[0].Index, loaded[1].Index)
	}
}

// TestFreshNode verifies that a brand-new store returns safe defaults
// (term=0, votedFor="", empty log) without errors.
func TestFreshNode(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	term, votedFor, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if term != 0 {
		t.Errorf("fresh term: want 0, got %d", term)
	}
	if votedFor != "" {
		t.Errorf("fresh votedFor: want '', got '%s'", votedFor)
	}

	entries, err := store.LoadLog()
	if err != nil {
		t.Fatalf("load log: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("fresh log: want 0 entries, got %d", len(entries))
	}
}
