package discovery

import (
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
)

// EventType classifies discovery events emitted to the Raft layer.
type EventType uint8

const (
	NodeJoin   EventType = iota // A new node appeared in the gossip mesh.
	NodeLeave                   // A node left or was detected as failed.
	NodeUpdate                  // A node's metadata was updated.
)

// String returns a human-readable event type label.
func (e EventType) String() string {
	switch e {
	case NodeJoin:
		return "JOIN"
	case NodeLeave:
		return "LEAVE"
	case NodeUpdate:
		return "UPDATE"
	default:
		return "UNKNOWN"
	}
}

// Event is emitted when the gossip mesh detects a membership change.
// The Raft layer consumes these to propose CONFIG_CHANGE log entries.
type Event struct {
	Type     EventType
	NodeID   string // memberlist Name — must match the slog node_id.
	RaftAddr string // The node's advertised gRPC/transport address.
}

// Config defines the parameters for a Discovery Manager.
type Config struct {
	NodeID   string // Must match the logger's node_id and Raft node identity.
	BindAddr string // Address to bind the gossip listener (e.g., "0.0.0.0", "127.0.0.1").
	BindPort int    // Port for gossip protocol. 0 = OS-assigned.
	RaftAddr string // The address to advertise for Raft RPCs.
	Logger   *slog.Logger
}

const defaultEventBuffer = 64

// Manager wraps HashiCorp memberlist and emits discovery events
// for the Raft consensus layer to consume.
type Manager struct {
	config Config
	list   *memberlist.Memberlist
	events chan Event
	logger *slog.Logger
	closed atomic.Bool
}

// New creates and starts a new Discovery Manager.
// The underlying memberlist is started immediately using LAN configuration.
// The Config.NodeID is used as the memberlist Name, ensuring it matches
// the slog logger's node_id for consistent correlation.
func New(cfg Config) (*Manager, error) {
	m := &Manager{
		config: cfg,
		events: make(chan Event, defaultEventBuffer),
		logger: cfg.Logger,
	}

	// LAN configuration: tuned for low-latency, same-datacenter gossip.
	mlConfig := memberlist.DefaultLANConfig()
	mlConfig.Name = cfg.NodeID
	mlConfig.BindAddr = cfg.BindAddr
	mlConfig.BindPort = cfg.BindPort
	mlConfig.AdvertisePort = cfg.BindPort

	// Wire delegates: metadata broadcast + event notifications.
	meta := NodeMeta{RaftAddr: cfg.RaftAddr}
	mlConfig.Delegate = &nodeDelegate{meta: meta.Encode()}
	mlConfig.Events = &eventDelegate{events: m.events}

	// Suppress memberlist's internal logging — we use slog exclusively.
	mlConfig.LogOutput = io.Discard

	list, err := memberlist.Create(mlConfig)
	if err != nil {
		return nil, fmt.Errorf("discovery: failed to create memberlist: %w", err)
	}
	m.list = list

	m.logger.Info("discovery mesh started",
		"bind_addr", cfg.BindAddr,
		"bind_port", list.LocalNode().Port,
		"raft_addr", cfg.RaftAddr,
	)

	return m, nil
}

// Join contacts the given seed addresses to join an existing cluster.
// Returns the number of nodes successfully contacted.
func (m *Manager) Join(seeds []string) (int, error) {
	n, err := m.list.Join(seeds)
	if err != nil {
		return n, fmt.Errorf("discovery: join failed: %w", err)
	}
	m.logger.Info("joined cluster", "contacted", n, "seeds", seeds)
	return n, nil
}

// Events returns a read-only channel of membership change events.
// The Raft layer consumes this channel to propose CONFIG_CHANGE entries.
// The channel is closed on Shutdown.
func (m *Manager) Events() <-chan Event {
	return m.events
}

// Members returns the list of known alive members in the gossip mesh.
func (m *Manager) Members() []*memberlist.Node {
	return m.list.Members()
}

// LocalNode returns the local node's memberlist representation.
func (m *Manager) LocalNode() *memberlist.Node {
	return m.list.LocalNode()
}

// MemberAddr returns the gossip address of this node as "host:port".
// Useful for constructing seed addresses for other nodes to join.
func (m *Manager) MemberAddr() string {
	node := m.list.LocalNode()
	return fmt.Sprintf("%s:%d", node.Addr, node.Port)
}

// Shutdown gracefully leaves the cluster and stops the gossip protocol.
// Blocks until the leave message propagates or the timeout expires.
// After Shutdown, the Events channel is closed and no further events are emitted.
func (m *Manager) Shutdown() error {
	if m.closed.Swap(true) {
		return nil // already shut down
	}

	// Give the leave notification time to propagate via gossip.
	if err := m.list.Leave(time.Second); err != nil {
		m.logger.Error("leave broadcast failed", "err", err)
	}

	err := m.list.Shutdown()
	close(m.events)

	m.logger.Info("discovery mesh shutdown")
	return err
}
