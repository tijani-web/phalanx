package network

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/basit-tijani/phalanx/pb"
)

const defaultInboxSize = 256

// LocalTransport is a channel-based in-memory transport for testing.
// It simulates node-to-node communication without network sockets
// and supports partition injection for fault-tolerance testing.
//
// Messages are deep-copied on Send to simulate the serialization
// boundary of a real network transport. This ensures tests catch
// data-race bugs that would surface in production.
type LocalTransport struct {
	addr  string
	inbox chan Message

	mu    sync.RWMutex
	peers map[string]*LocalTransport

	// Partition simulation: blocked target addresses.
	partMu      sync.RWMutex
	partitioned map[string]struct{}

	closed atomic.Bool
}

// NewLocalTransport creates a new in-memory transport bound to the given address.
func NewLocalTransport(addr string) *LocalTransport {
	return &LocalTransport{
		addr:        addr,
		inbox:       make(chan Message, defaultInboxSize),
		peers:       make(map[string]*LocalTransport),
		partitioned: make(map[string]struct{}),
	}
}

// Connect establishes a bidirectional link between two local transports.
// Both a and b can Send to each other after this call.
func Connect(a, b *LocalTransport) {
	a.mu.Lock()
	a.peers[b.addr] = b
	a.mu.Unlock()

	b.mu.Lock()
	b.peers[a.addr] = a
	b.mu.Unlock()
}

// Disconnect removes the bidirectional link between two local transports.
func Disconnect(a, b *LocalTransport) {
	a.mu.Lock()
	delete(a.peers, b.addr)
	a.mu.Unlock()

	b.mu.Lock()
	delete(b.peers, a.addr)
	b.mu.Unlock()
}

// Partition simulates a unidirectional network partition: this node
// can no longer send to the target. The target can still send to this node.
func (t *LocalTransport) Partition(target string) {
	t.partMu.Lock()
	t.partitioned[target] = struct{}{}
	t.partMu.Unlock()
}

// Heal removes a simulated partition to the target.
func (t *LocalTransport) Heal(target string) {
	t.partMu.Lock()
	delete(t.partitioned, target)
	t.partMu.Unlock()
}

// Send delivers a deep copy of the entry to the target node's inbox.
// The caller retains ownership of the original entry.
func (t *LocalTransport) Send(ctx context.Context, target string, entry *pb.LogEntry) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}

	// Check partition before peer lookup — fail fast.
	t.partMu.RLock()
	_, blocked := t.partitioned[target]
	t.partMu.RUnlock()
	if blocked {
		return ErrPartitioned
	}

	// Resolve peer.
	t.mu.RLock()
	peer, ok := t.peers[target]
	t.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrPeerNotFound, target)
	}

	// Hard copy via pool to simulate the serialization boundary.
	// Uses make+copy instead of append to guarantee the clone's Data
	// backing array is fully independent of the caller's memory.
	// In the gRPC transport, this isolation happens naturally via protobuf encoding.
	clone := AcquireEntry()
	clone.Index = entry.Index
	clone.Term = entry.Term
	clone.Type = entry.Type
	if len(entry.Data) > 0 {
		clone.Data = make([]byte, len(entry.Data))
		copy(clone.Data, entry.Data)
	} else {
		clone.Data = clone.Data[:0]
	}

	msg := Message{
		From:  t.addr,
		Entry: clone,
	}

	select {
	case peer.inbox <- msg:
		return nil
	case <-ctx.Done():
		ReleaseEntry(clone)
		return ctx.Err()
	}
}

// Consume returns the channel for receiving inbound messages.
// The channel is closed when the transport is shut down.
func (t *LocalTransport) Consume() <-chan Message {
	return t.inbox
}

// Addr returns this transport's address identifier.
func (t *LocalTransport) Addr() string {
	return t.addr
}

// Close shuts down the transport, closing the inbox channel.
// Returns ErrTransportClosed if already closed.
func (t *LocalTransport) Close() error {
	if t.closed.Swap(true) {
		return ErrTransportClosed
	}
	close(t.inbox)
	return nil
}

// Reset wipes all peer connections, partitions, and drains the inbox.
// Intended for use between test cases to ensure clean state.
// The transport remains open and usable after Reset.
func (t *LocalTransport) Reset() {
	t.mu.Lock()
	t.peers = make(map[string]*LocalTransport)
	t.mu.Unlock()

	t.partMu.Lock()
	t.partitioned = make(map[string]struct{})
	t.partMu.Unlock()

	// Drain inbox — return entries to pool.
	for {
		select {
		case msg, ok := <-t.inbox:
			if !ok {
				return
			}
			ReleaseEntry(msg.Entry)
		default:
			return
		}
	}
}
