// Package phalanx provides the top-level Node that wires together
// the Raft state machine, gRPC transport, gossip discovery,
// BadgerDB persistence, KV FSM, and observability into a single event loop.
//
// The Node is the "glue" — it owns the main select loop:
//   - time.Ticker      → raft.Tick() → dispatch heartbeats/votes
//   - gRPC consensus   → raft.Step() → respond to peers
//   - gRPC KV Propose  → raft.Propose() → wait for commit → apply to FSM
//   - gRPC KV Read     → HasLeaderQuorum() → read from FSM
//   - Discovery        → ProposeConfigChange() → cluster membership
//   - Async responses  → raft.Step() → process peer replies
package phalanx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/tijani-web/phalanx/discovery"
	"github.com/tijani-web/phalanx/fsm"
	"github.com/tijani-web/phalanx/network"
	"github.com/tijani-web/phalanx/observability"
	"github.com/tijani-web/phalanx/pb"
	"github.com/tijani-web/phalanx/raft"
	"github.com/tijani-web/phalanx/storage"
)

// NodeConfig defines the parameters for a Phalanx node.
type NodeConfig struct {
	ID               string
	Peers            []string
	TickInterval     time.Duration // How often to call raft.Tick() (e.g., 100ms).
	ElectionTimeout  int           // Base election timeout in ticks.
	HeartbeatTimeout int           // Heartbeat interval in ticks.
	DataDir          string        // Path for BadgerDB storage.
	GRPCAddr         string        // gRPC listen address (default: "[::]:9000").
	DebugAddr        string        // Debug HTTP address (default: ":8080").
	Logger           *slog.Logger
	Term             *atomic.Uint64
}

// Node is the top-level Phalanx consensus node.
// It orchestrates the Raft state machine, transport, discovery,
// persistence, KV FSM, and observability through a single-threaded event loop.
type Node struct {
	raft      *raft.Raft
	grpc      *network.GRPCTransport
	discovery *discovery.Manager
	store     *storage.Store
	fsm       *fsm.KV
	metrics   *observability.Metrics
	logger    *slog.Logger
	cfg       NodeConfig

	// Async responses from outgoing gRPC calls are fed back here.
	responseCh chan raft.Message

	// Pending proposals awaiting commit — keyed by log index.
	proposals map[uint64]chan struct{}

	// Peer address map: node ID → gRPC address.
	// Populated by SetPeerAddr or discovery.
	peerAddrs map[string]string
}

// NewNode creates a new Phalanx node with persistence, gRPC transport,
// KV FSM, and observability.
//
// Lifecycle:
//  1. Opens BadgerDB and loads persisted state.
//  2. Creates the Raft state machine with restored state.
//  3. Starts the gRPC transport server.
//  4. Returns a Node ready for Run().
func NewNode(cfg NodeConfig) (*Node, error) {
	// --- Storage ---
	store, err := storage.New(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("node: storage: %w", err)
	}

	// Load persisted state (safe defaults for fresh node).
	savedTerm, savedVote, err := store.LoadState()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("node: load state: %w", err)
	}

	savedLog, err := store.LoadLog()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("node: load log: %w", err)
	}

	// --- Raft ---
	r := raft.NewRaft(raft.Config{
		ID:               cfg.ID,
		Peers:            cfg.Peers,
		ElectionTimeout:  cfg.ElectionTimeout,
		HeartbeatTimeout: cfg.HeartbeatTimeout,
		Logger:           cfg.Logger,
		Term:             cfg.Term,
		InitialTerm:      savedTerm,
		InitialVotedFor:  savedVote,
		InitialLog:       savedLog,
	})

	// --- gRPC Transport ---
	grpcTransport, err := network.NewGRPCTransport(network.GRPCConfig{
		Addr:   cfg.GRPCAddr,
		Logger: cfg.Logger,
	})
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("node: transport: %w", err)
	}

	n := &Node{
		raft:       r,
		grpc:       grpcTransport,
		store:      store,
		fsm:        fsm.NewKV(),
		metrics:    observability.NewMetrics(),
		logger:     cfg.Logger,
		cfg:        cfg,
		responseCh: make(chan raft.Message, 128),
		proposals:  make(map[uint64]chan struct{}),
		peerAddrs:  make(map[string]string),
	}

	cfg.Logger.Info("node initialized",
		"id", cfg.ID,
		"peers", cfg.Peers,
		"data_dir", cfg.DataDir,
		"grpc_addr", grpcTransport.Addr(),
	)

	return n, nil
}

// SetDiscovery attaches a gossip discovery manager to the Node.
// Must be called before Run(). Optional — the Node runs without
// discovery (useful for testing or static cluster configurations).
func (n *Node) SetDiscovery(d *discovery.Manager) {
	n.discovery = d
}

// FSM returns the KV state machine for direct reads (e.g., testing).
func (n *Node) FSM() *fsm.KV { return n.fsm }

// Raft returns the underlying Raft state machine.
func (n *Node) Raft() *raft.Raft { return n.raft }

// GRPCAddr returns the gRPC listen address.
func (n *Node) GRPCAddr() string { return n.grpc.Addr() }

// SetPeerAddr registers the gRPC address for a peer node ID.
// Must be called before Run() or from the discovery event handler.
func (n *Node) SetPeerAddr(nodeID, addr string) {
	n.peerAddrs[nodeID] = addr
}

// resolvePeerAddr returns the gRPC address for a node ID.
func (n *Node) resolvePeerAddr(nodeID string) string {
	if addr, ok := n.peerAddrs[nodeID]; ok {
		return addr
	}
	return nodeID // Fallback: assume the ID IS the address.
}

// Run starts the main event loop. Blocks until the context is cancelled
// or a fatal error occurs. This is the heart of the Phalanx node.
func (n *Node) Run(ctx context.Context) error {
	// Start the debug HTTP server on a separate port.
	go n.startDebugServer()

	ticker := time.NewTicker(n.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return n.shutdown()

		// --- Tick ---
		// Advances the Raft logical clock. Followers check election timeout,
		// leaders send heartbeats. Post-tick messages are dispatched.
		case <-ticker.C:
			n.raft.Tick()
			n.applyCommitted()
			n.persistState()
			n.dispatchMessages()
			n.updateMetrics()

		// --- Incoming gRPC Consensus RPCs ---
		case rpc, ok := <-n.grpc.RPCs():
			if !ok {
				return fmt.Errorf("node: rpc channel closed")
			}
			n.handleRPC(rpc)

		// --- Client Propose ---
		case op := <-n.grpc.Proposes():
			n.handlePropose(op)

		// --- Client Read ---
		case op := <-n.grpc.Reads():
			n.handleRead(op)

		// --- Async Responses ---
		case resp := <-n.responseCh:
			n.raft.Step(resp)
			n.applyCommitted()
			n.persistState()
			n.dispatchMessages()
			n.updateMetrics()

		// --- Discovery Events ---
		case event, ok := <-n.discoveryEvents():
			if !ok {
				continue
			}
			n.handleDiscoveryEvent(event)
		}
	}
}

// ---------------------------------------------------------------------------
// KV Handlers
// ---------------------------------------------------------------------------

func (n *Node) handlePropose(op network.ProposeOp) {
	n.metrics.ProposalsTotal.Add(1)

	// Leader redirection.
	if n.raft.State() != raft.Leader {
		op.Response <- &pb.ProposeResponse{
			Success:    false,
			LeaderAddr: n.resolveLeaderAddr(),
			Error:      "not leader",
		}
		return
	}

	// Propose to Raft log.
	idx, err := n.raft.Propose(op.Request.Data)
	if err != nil {
		op.Response <- &pb.ProposeResponse{
			Success: false,
			Error:   err.Error(),
		}
		return
	}

	// Register pending proposal — will be signalled when applied.
	doneCh := make(chan struct{})
	n.proposals[idx] = doneCh

	n.persistState()
	n.dispatchMessages()
	n.updateMetrics()

	// Wait for commit asynchronously to avoid blocking the event loop.
	go func() {
		select {
		case <-doneCh:
			op.Response <- &pb.ProposeResponse{Success: true}
		case <-time.After(5 * time.Second):
			op.Response <- &pb.ProposeResponse{
				Success: false,
				Error:   "proposal timed out",
			}
		}
	}()
}

func (n *Node) handleRead(op network.ReadOp) {
	n.metrics.ReadsTotal.Add(1)

	// Leader redirection.
	if n.raft.State() != raft.Leader {
		op.Response <- &pb.ReadResponse{
			LeaderAddr: n.resolveLeaderAddr(),
			Error:      "not leader",
		}
		return
	}

	// Linearizable read: verify we still hold a majority lease.
	if !n.raft.HasLeaderQuorum() {
		op.Response <- &pb.ReadResponse{
			Error: "leader lost quorum — cannot serve linearizable read",
		}
		return
	}

	// Read from FSM.
	value, found := n.fsm.Get(op.Request.Key)
	op.Response <- &pb.ReadResponse{
		Value: value,
		Found: found,
	}
}

// resolveLeaderAddr returns the leader's gRPC address for client redirection.
// For now, returns the leader node ID; in production, the discovery layer
// would map node IDs to gRPC addresses.
func (n *Node) resolveLeaderAddr() string {
	leaderID := n.raft.Leader()
	if addr, ok := n.peerAddrs[leaderID]; ok {
		return addr
	}
	return leaderID
}

// ---------------------------------------------------------------------------
// FSM Application
// ---------------------------------------------------------------------------

// applyCommitted applies all newly committed entries to the KV FSM.
// Also signals any pending proposals that are now committed.
func (n *Node) applyCommitted() {
	entries := n.raft.ApplicableEntries()
	for _, entry := range entries {
		// Apply to FSM (skip no-ops and config changes).
		if entry.Type == pb.EntryCommand && len(entry.Data) > 0 {
			if err := n.fsm.Apply(entry.Data); err != nil {
				n.logger.Error("fsm apply failed",
					"index", entry.Index,
					"err", err,
				)
			}
		}

		// Signal pending proposal.
		if ch, ok := n.proposals[entry.Index]; ok {
			close(ch)
			delete(n.proposals, entry.Index)
		}

		n.metrics.AppliedIndex.Store(entry.Index)
	}
}

// ---------------------------------------------------------------------------
// Consensus Event Handlers
// ---------------------------------------------------------------------------

func (n *Node) handleRPC(rpc network.IncomingRPC) {
	if rpc.AppendEntries != nil {
		n.handleAppendEntriesRPC(rpc.AppendEntries)
	} else if rpc.RequestVote != nil {
		n.handleRequestVoteRPC(rpc.RequestVote)
	}
}

func (n *Node) handleAppendEntriesRPC(rpc *network.AppendEntriesRPC) {
	msg := raft.Message{
		Type:         raft.MsgAppendEntries,
		From:         rpc.Request.LeaderID,
		To:           n.cfg.ID,
		Term:         rpc.Request.Term,
		PrevLogIndex: rpc.Request.PrevLogIndex,
		PrevLogTerm:  rpc.Request.PrevLogTerm,
		Entries:      rpc.Request.Entries,
		LeaderCommit: rpc.Request.LeaderCommit,
	}

	n.raft.Step(msg)
	n.applyCommitted()
	n.persistState()

	// Extract response and route remaining messages.
	for _, m := range n.raft.Messages() {
		if m.Type == raft.MsgAppendEntriesResp && m.To == msg.From {
			rpc.Response <- &pb.AppendEntriesResponse{
				Term:         m.Term,
				Success:      !m.Reject,
				LastLogIndex: m.Index,
			}
		} else {
			n.routeMessage(m)
		}
	}
	n.updateMetrics()
}

func (n *Node) handleRequestVoteRPC(rpc *network.RequestVoteRPC) {
	msg := raft.Message{
		Type:         raft.MsgRequestVote,
		From:         rpc.Request.CandidateID,
		To:           n.cfg.ID,
		Term:         rpc.Request.Term,
		LastLogIndex: rpc.Request.LastLogIndex,
		LastLogTerm:  rpc.Request.LastLogTerm,
		IsPreVote:    rpc.Request.IsPreVote,
	}

	n.raft.Step(msg)
	n.persistState()

	for _, m := range n.raft.Messages() {
		if m.Type == raft.MsgRequestVoteResp && m.To == msg.From {
			rpc.Response <- &pb.RequestVoteResponse{
				Term:        m.Term,
				VoteGranted: !m.Reject,
				IsPreVote:   m.IsPreVote,
			}
		} else {
			n.routeMessage(m)
		}
	}
	n.updateMetrics()
}

func (n *Node) handleDiscoveryEvent(event discovery.Event) {
	switch event.Type {
	case discovery.NodeJoin:
		n.logger.Info("node joined cluster",
			"node_id", event.NodeID,
			"raft_addr", event.RaftAddr,
		)
		n.raft.AddPeer(event.NodeID)
		n.peerAddrs[event.NodeID] = event.RaftAddr
		n.persistState()
		n.dispatchMessages()

	case discovery.NodeLeave:
		n.logger.Info("node left cluster",
			"node_id", event.NodeID,
			"raft_addr", event.RaftAddr,
		)
	}
}

// ---------------------------------------------------------------------------
// Message Dispatch
// ---------------------------------------------------------------------------

func (n *Node) dispatchMessages() {
	for _, msg := range n.raft.Messages() {
		n.metrics.MessagesSent.Add(1)
		n.routeMessage(msg)
	}
}

func (n *Node) routeMessage(msg raft.Message) {
	switch msg.Type {
	case raft.MsgAppendEntries:
		go n.sendAppendEntries(msg)
	case raft.MsgRequestVote:
		go n.sendRequestVote(msg)
	default:
		n.logger.Warn("unroutable message", "type", msg.Type, "to", msg.To)
	}
}

func (n *Node) sendAppendEntries(msg raft.Message) {
	req := &pb.AppendEntriesRequest{
		Term:         msg.Term,
		LeaderID:     msg.From,
		PrevLogIndex: msg.PrevLogIndex,
		PrevLogTerm:  msg.PrevLogTerm,
		Entries:      msg.Entries,
		LeaderCommit: msg.LeaderCommit,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := n.grpc.SendAppendEntries(ctx, n.resolvePeerAddr(msg.To), req)
	if err != nil {
		n.logger.Debug("send AE failed", "to", msg.To, "err", err)
		return
	}

	n.responseCh <- raft.Message{
		Type:   raft.MsgAppendEntriesResp,
		From:   msg.To,
		To:     msg.From,
		Term:   resp.Term,
		Reject: !resp.Success,
		Index:  resp.LastLogIndex,
	}
}

func (n *Node) sendRequestVote(msg raft.Message) {
	req := &pb.RequestVoteRequest{
		Term:         msg.Term,
		CandidateID:  msg.From,
		LastLogIndex: msg.LastLogIndex,
		LastLogTerm:  msg.LastLogTerm,
		IsPreVote:    msg.IsPreVote,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := n.grpc.SendRequestVote(ctx, n.resolvePeerAddr(msg.To), req)
	if err != nil {
		n.logger.Debug("send RV failed", "to", msg.To, "err", err)
		return
	}

	n.responseCh <- raft.Message{
		Type:      raft.MsgRequestVoteResp,
		From:      msg.To,
		To:        msg.From,
		Term:      resp.Term,
		Reject:    !resp.VoteGranted,
		IsPreVote: resp.IsPreVote,
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (n *Node) persistState() {
	if err := n.store.SaveState(n.raft.CurrentTerm(), n.raft.VotedFor()); err != nil {
		n.logger.Error("persist state failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

func (n *Node) updateMetrics() {
	n.metrics.LastCommitIndex.Store(n.raft.CommitIndex())
	n.metrics.CurrentState.Store(n.raft.State().String())
}

func (n *Node) nodeStatus() observability.NodeStatus {
	return observability.NodeStatus{
		NodeID:     n.cfg.ID,
		State:      n.raft.State().String(),
		Term:       n.raft.CurrentTerm(),
		LeaderID:   n.raft.Leader(),
		CommitIdx:  n.raft.CommitIndex(),
		AppliedIdx: n.raft.LastApplied(),
		LogLength:  len(n.raft.Log()),
		Peers:      n.raft.Peers(),
		KVSize:     n.fsm.Len(),
		KVData:     n.fsm.Snapshot(),
		Metrics:    n.metrics.Snapshot(),
	}
}

func (n *Node) startDebugServer() {
	addr := n.cfg.DebugAddr
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.Handle("/debug/status", observability.DebugHandler(n.nodeStatus))

	// Health check returns JSON status too.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"node":   n.cfg.ID,
			"state":  n.raft.State().String(),
		})
	})

	n.logger.Info("debug server started", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		n.logger.Error("debug server error", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (n *Node) discoveryEvents() <-chan discovery.Event {
	if n.discovery == nil {
		return nil
	}
	return n.discovery.Events()
}

func (n *Node) shutdown() error {
	n.logger.Info("node shutting down")
	if n.grpc != nil {
		n.grpc.Close()
	}
	if n.store != nil {
		n.store.Close()
	}
	return nil
}
