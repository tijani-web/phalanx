// Package network provides the transport abstraction for Phalanx
// node-to-node communication.
//
// The Transport interface is designed to be swappable:
//   - LocalTransport (channels) for deterministic testing and partition simulation.
//   - GRPCTransport (wire) for production deployment (Phase 3).
package network

import (
	"context"
	"errors"

	"github.com/basit-tijani/phalanx/pb"
)

// Sentinel errors for transport operations.
var (
	ErrTransportClosed = errors.New("transport: closed")
	ErrPeerNotFound    = errors.New("transport: peer not found")
	ErrPartitioned     = errors.New("transport: network partition")
)

// Message wraps an inbound log entry with sender metadata.
// Receivers must call ReleaseEntry(msg.Entry) after processing
// to return the entry to the pool.
type Message struct {
	From  string
	Entry *pb.LogEntry
}

// Transport abstracts node-to-node communication.
// All implementations must be safe for concurrent use by multiple goroutines.
type Transport interface {
	// Send delivers a log entry to the target node.
	// The implementation MUST deep-copy the entry — callers retain ownership.
	Send(ctx context.Context, target string, entry *pb.LogEntry) error

	// Consume returns a read-only channel of inbound messages.
	// The channel is closed when the transport shuts down.
	Consume() <-chan Message

	// Addr returns the local address identifier of this transport.
	Addr() string

	// Close shuts down the transport and releases all resources.
	// After Close returns, Send will return ErrTransportClosed and
	// the Consume channel will be drained and closed.
	Close() error
}
