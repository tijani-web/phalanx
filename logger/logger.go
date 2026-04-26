// Package logger provides a structured slog logger for Phalanx nodes.
//
// The logger injects node_id and the current Raft term into every log
// record. The term is read from an atomic value so it always reflects
// the latest term without requiring logger re-creation on elections.
package logger

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
)

// PhalanxHandler wraps an slog.Handler to inject node_id and the
// current Raft term into every log record automatically.
type PhalanxHandler struct {
	inner  slog.Handler
	nodeID string
	term   *atomic.Uint64
}

// Enabled reports whether the handler handles records at the given level.
func (h *PhalanxHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds node_id and the current term to every log record.
// This is called on the hot path — the atomic load is a single
// cache-line read with no contention.
func (h *PhalanxHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(
		slog.String("node_id", h.nodeID),
		slog.Uint64("term", h.term.Load()),
	)
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a new handler that includes the given attributes.
func (h *PhalanxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &PhalanxHandler{
		inner:  h.inner.WithAttrs(attrs),
		nodeID: h.nodeID,
		term:   h.term,
	}
}

// WithGroup returns a new handler with the given group name.
func (h *PhalanxHandler) WithGroup(name string) slog.Handler {
	return &PhalanxHandler{
		inner:  h.inner.WithGroup(name),
		nodeID: h.nodeID,
		term:   h.term,
	}
}

// New creates a structured JSON logger bound to a specific node.
//
// The term pointer is shared with the Raft state machine — when the
// node's term advances (elections, AppendEntries), the logger
// automatically reflects the new term without reconstruction.
//
// Usage:
//
//	term := &atomic.Uint64{}
//	log := logger.New("node-1", term)
//	log.Info("started", "addr", "127.0.0.1:9000")
//	// Output: {"level":"INFO","msg":"started","node_id":"node-1","term":0,"addr":"127.0.0.1:9000"}
//
//	term.Store(3) // election happened
//	log.Info("elected leader")
//	// Output: {"level":"INFO","msg":"elected leader","node_id":"node-1","term":3}
func New(nodeID string, term *atomic.Uint64) *slog.Logger {
	handler := &PhalanxHandler{
		inner: slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}),
		nodeID: nodeID,
		term:   term,
	}
	return slog.New(handler)
}
