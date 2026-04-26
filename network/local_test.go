package network

import (
	"context"
	"testing"
	"time"

	"github.com/tijani-web/phalanx/pb"
)

// TestLocalTransport_SendReceive proves Node A can send a LogEntry
// to Node B without using an actual network socket.
func TestLocalTransport_SendReceive(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	nodeB := NewLocalTransport("node-b")
	defer nodeA.Close()
	defer nodeB.Close()

	Connect(nodeA, nodeB)

	// Prepare entry from pool — zero-alloc pattern.
	entry := AcquireEntry()
	entry.Index = 1
	entry.Term = 3
	entry.Type = pb.EntryCommand
	entry.Data = append(entry.Data, []byte("SET x=42")...)
	defer ReleaseEntry(entry)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := nodeA.Send(ctx, "node-b", entry); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	select {
	case msg := <-nodeB.Consume():
		if msg.From != "node-a" {
			t.Errorf("From: want node-a, got %s", msg.From)
		}
		if msg.Entry.Index != 1 {
			t.Errorf("Index: want 1, got %d", msg.Entry.Index)
		}
		if msg.Entry.Term != 3 {
			t.Errorf("Term: want 3, got %d", msg.Entry.Term)
		}
		if msg.Entry.Type != pb.EntryCommand {
			t.Errorf("Type: want COMMAND(0), got %d", msg.Entry.Type)
		}
		if string(msg.Entry.Data) != "SET x=42" {
			t.Errorf("Data: want 'SET x=42', got '%s'", msg.Entry.Data)
		}
		// Critical: verify deep copy — not the same pointer.
		if msg.Entry == entry {
			t.Error("received entry is same pointer as sent — must be deep copy")
		}
		ReleaseEntry(msg.Entry)
	case <-ctx.Done():
		t.Fatal("timed out waiting for message on node-b")
	}
}

// TestLocalTransport_Partition verifies network partition simulation.
func TestLocalTransport_Partition(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	nodeB := NewLocalTransport("node-b")
	defer nodeA.Close()
	defer nodeB.Close()

	Connect(nodeA, nodeB)

	// Inject partition: A cannot reach B.
	nodeA.Partition("node-b")

	entry := AcquireEntry()
	entry.Index = 1
	entry.Term = 1
	defer ReleaseEntry(entry)

	ctx := context.Background()
	err := nodeA.Send(ctx, "node-b", entry)
	if err != ErrPartitioned {
		t.Fatalf("expected ErrPartitioned, got: %v", err)
	}

	// Heal partition — send should succeed.
	nodeA.Heal("node-b")

	if err := nodeA.Send(ctx, "node-b", entry); err != nil {
		t.Fatalf("Send after heal failed: %v", err)
	}

	select {
	case msg := <-nodeB.Consume():
		if msg.Entry.Index != 1 {
			t.Errorf("Index: want 1, got %d", msg.Entry.Index)
		}
		ReleaseEntry(msg.Entry)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message after healing partition")
	}
}

// TestLocalTransport_UnknownPeer verifies error on send to unknown target.
func TestLocalTransport_UnknownPeer(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	defer nodeA.Close()

	entry := AcquireEntry()
	entry.Index = 1
	defer ReleaseEntry(entry)

	err := nodeA.Send(context.Background(), "ghost", entry)
	if err == nil {
		t.Fatal("expected error for unknown peer, got nil")
	}
}

// TestLocalTransport_ClosedSend verifies ErrTransportClosed after Close().
func TestLocalTransport_ClosedSend(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	nodeB := NewLocalTransport("node-b")
	Connect(nodeA, nodeB)
	defer nodeB.Close()

	nodeA.Close()

	entry := AcquireEntry()
	entry.Index = 1
	defer ReleaseEntry(entry)

	err := nodeA.Send(context.Background(), "node-b", entry)
	if err != ErrTransportClosed {
		t.Fatalf("expected ErrTransportClosed, got: %v", err)
	}
}

// TestPool_AcquireRelease verifies the sync.Pool lifecycle and Reset behavior.
func TestPool_AcquireRelease(t *testing.T) {
	e := AcquireEntry()
	e.Index = 99
	e.Term = 7
	e.Data = append(e.Data, []byte("hello")...)
	e.Type = pb.EntryConfigChange

	ReleaseEntry(e)

	// Pool may or may not return the same object, but if it does,
	// fields must be zeroed by Reset().
	e2 := AcquireEntry()
	if e2.Index != 0 || e2.Term != 0 || len(e2.Data) != 0 || e2.Type != pb.EntryCommand {
		t.Error("pooled entry was not properly reset")
	}
	ReleaseEntry(e2)
}

// TestLocalTransport_Bidirectional verifies both directions of communication.
func TestLocalTransport_Bidirectional(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	nodeB := NewLocalTransport("node-b")
	defer nodeA.Close()
	defer nodeB.Close()

	Connect(nodeA, nodeB)
	ctx := context.Background()

	// A -> B
	entryAB := AcquireEntry()
	entryAB.Index = 1
	entryAB.Term = 1
	entryAB.Data = append(entryAB.Data, []byte("A->B")...)
	defer ReleaseEntry(entryAB)

	if err := nodeA.Send(ctx, "node-b", entryAB); err != nil {
		t.Fatalf("A->B send failed: %v", err)
	}

	// B -> A
	entryBA := AcquireEntry()
	entryBA.Index = 2
	entryBA.Term = 1
	entryBA.Data = append(entryBA.Data, []byte("B->A")...)
	defer ReleaseEntry(entryBA)

	if err := nodeB.Send(ctx, "node-a", entryBA); err != nil {
		t.Fatalf("B->A send failed: %v", err)
	}

	// Verify both received
	select {
	case msg := <-nodeB.Consume():
		if string(msg.Entry.Data) != "A->B" {
			t.Errorf("B received wrong data: %s", msg.Entry.Data)
		}
		ReleaseEntry(msg.Entry)
	case <-time.After(time.Second):
		t.Fatal("B did not receive message from A")
	}

	select {
	case msg := <-nodeA.Consume():
		if string(msg.Entry.Data) != "B->A" {
			t.Errorf("A received wrong data: %s", msg.Entry.Data)
		}
		ReleaseEntry(msg.Entry)
	case <-time.After(time.Second):
		t.Fatal("A did not receive message from B")
	}
}

// TestLocalTransport_IPv6 verifies transport handles IPv6 bracket notation
// required for Fly.io's private WireGuard mesh (6PN addressing).
func TestLocalTransport_IPv6(t *testing.T) {
	nodeA := NewLocalTransport("[::1]:9000")
	nodeB := NewLocalTransport("[fdaa:0:1::3]:9001")
	defer nodeA.Close()
	defer nodeB.Close()

	Connect(nodeA, nodeB)

	entry := AcquireEntry()
	entry.Index = 1
	entry.Term = 1
	entry.Data = append(entry.Data[:0], []byte("ipv6-test")...)
	defer ReleaseEntry(entry)

	// Send A -> B using full IPv6 bracket notation as target.
	if err := nodeA.Send(context.Background(), "[fdaa:0:1::3]:9001", entry); err != nil {
		t.Fatalf("IPv6 send failed: %v", err)
	}

	select {
	case msg := <-nodeB.Consume():
		if msg.From != "[::1]:9000" {
			t.Errorf("From: want [::1]:9000, got %s", msg.From)
		}
		if string(msg.Entry.Data) != "ipv6-test" {
			t.Errorf("Data: want 'ipv6-test', got '%s'", msg.Entry.Data)
		}
		ReleaseEntry(msg.Entry)
	case <-time.After(time.Second):
		t.Fatal("IPv6 receive timed out")
	}
}

// TestLocalTransport_Reset verifies Reset wipes peers, partitions, and inbox.
func TestLocalTransport_Reset(t *testing.T) {
	nodeA := NewLocalTransport("node-a")
	nodeB := NewLocalTransport("node-b")
	defer nodeA.Close()
	defer nodeB.Close()

	Connect(nodeA, nodeB)

	// Send a message so inbox has content.
	entry := AcquireEntry()
	entry.Index = 1
	entry.Term = 1
	defer ReleaseEntry(entry)

	if err := nodeA.Send(context.Background(), "node-b", entry); err != nil {
		t.Fatalf("pre-reset send failed: %v", err)
	}

	// Reset node B.
	nodeB.Reset()

	// Peers should be wiped — A can no longer send because B no longer knows A.
	// But A still has B as a peer, so A can still send. The point is B's state is clean.

	// Inbox should be drained.
	select {
	case msg := <-nodeB.Consume():
		t.Fatalf("inbox should be empty after Reset, got message from %s", msg.From)
	default:
		// Expected: inbox is empty.
	}

	// Peers should be gone from B's side.
	err := nodeB.Send(context.Background(), "node-a", entry)
	if err == nil {
		t.Fatal("expected error sending to wiped peer list, got nil")
	}
}
