// Package observability provides production metrics and a debug HTTP endpoint
// for the Phalanx consensus node.
//
// The debug UI runs on a separate port (default :8080) and serves a JSON
// summary of the node's health, cluster view, and KV state.
package observability

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// Metrics tracks production counters for a Phalanx node.
// All fields are atomically updated — safe for concurrent reads from
// the debug HTTP handler while the Node event loop writes.
type Metrics struct {
	MessagesSent    atomic.Uint64
	ElectionCount   atomic.Uint64
	LastCommitIndex atomic.Uint64
	ProposalsTotal  atomic.Uint64
	ReadsTotal      atomic.Uint64
	AppliedIndex    atomic.Uint64
	CurrentState    atomic.Value // stores string: "follower", "candidate", "leader"
}

// NewMetrics creates a zero-valued Metrics with state set to "follower".
func NewMetrics() *Metrics {
	m := &Metrics{}
	m.CurrentState.Store("follower")
	return m
}

// MetricsSnapshot is a JSON-serializable point-in-time capture of Metrics.
type MetricsSnapshot struct {
	MessagesSent    uint64 `json:"messages_sent_total"`
	ElectionCount   uint64 `json:"election_count"`
	LastCommitIndex uint64 `json:"last_commit_index"`
	ProposalsTotal  uint64 `json:"proposals_total"`
	ReadsTotal      uint64 `json:"reads_total"`
	AppliedIndex    uint64 `json:"applied_index"`
	CurrentState    string `json:"current_state"`
}

// Snapshot returns a JSON-safe capture of all metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		MessagesSent:    m.MessagesSent.Load(),
		ElectionCount:   m.ElectionCount.Load(),
		LastCommitIndex: m.LastCommitIndex.Load(),
		ProposalsTotal:  m.ProposalsTotal.Load(),
		ReadsTotal:      m.ReadsTotal.Load(),
		AppliedIndex:    m.AppliedIndex.Load(),
		CurrentState:    m.CurrentState.Load().(string),
	}
}

// NodeStatus is the full JSON payload served by the debug endpoint.
type NodeStatus struct {
	NodeID     string            `json:"node_id"`
	State      string            `json:"state"`
	Term       uint64            `json:"term"`
	LeaderID   string            `json:"leader_id"`
	CommitIdx  uint64            `json:"commit_index"`
	AppliedIdx uint64            `json:"applied_index"`
	LogLength  int               `json:"log_length"`
	Peers      []string          `json:"peers"`
	KVSize     int               `json:"kv_size"`
	KVData     map[string]string `json:"kv_data,omitempty"`
	Metrics    MetricsSnapshot   `json:"metrics"`
}

// StatusFunc is a callback the Node provides to produce its current status.
// Avoids circular imports between observability and the root phalanx package.
type StatusFunc func() NodeStatus

// DebugHandler returns an HTTP handler that serves the node's live status
// as JSON. The Node passes a closure that captures its internal state.
func DebugHandler(statusFn StatusFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		status := statusFn()
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(status)
	})
}
