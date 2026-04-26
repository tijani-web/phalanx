package phalanx

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tijani-web/phalanx/fsm"
	"github.com/tijani-web/phalanx/network"
	"github.com/tijani-web/phalanx/pb"
	"github.com/tijani-web/phalanx/raft"
)

// TestClientKV starts a 3-node cluster over real gRPC, elects a leader,
// sends SET "foo"="bar" to the leader via the KV Propose RPC,
// and verifies that all three nodes eventually apply the entry to their
// KV state machines.
//
// This is the end-to-end integration test for the full Phalanx stack:
// gRPC → Node event loop → Raft → log replication → FSM application.
func TestClientKV(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const (
		numNodes     = 3
		tickInterval = 20 * time.Millisecond
	)

	type testNode struct {
		node   *Node
		cancel context.CancelFunc
	}

	nodes := make([]*testNode, numNodes)

	// Build peer lists: node IDs are "node-0", "node-1", "node-2".
	// gRPC addresses are assigned dynamically with port 0.
	ids := make([]string, numNodes)
	for i := range ids {
		ids[i] = fmt.Sprintf("node-%d", i)
	}

	// --- Phase 1: Create nodes ---
	for i := 0; i < numNodes; i++ {
		peers := make([]string, 0, numNodes-1)
		for j := 0; j < numNodes; j++ {
			if j != i {
				peers = append(peers, ids[j])
			}
		}

		term := &atomic.Uint64{}
		logger := slog.Default().With("node_id", ids[i])

		n, err := NewNode(NodeConfig{
			ID:               ids[i],
			Peers:            peers,
			TickInterval:     tickInterval,
			ElectionTimeout:  10,
			HeartbeatTimeout: 3,
			DataDir:          t.TempDir(),
			GRPCAddr:         "127.0.0.1:0", // OS-assigned port.
			DebugAddr:        "127.0.0.1:0",  // OS-assigned port.
			Logger:           logger,
			Term:             term,
		})
		if err != nil {
			t.Fatalf("create node %d: %v", i, err)
		}

		nodes[i] = &testNode{node: n}
	}

	defer func() {
		for _, tn := range nodes {
			tn.cancel()
		}
		// Give goroutines time to clean up.
		time.Sleep(100 * time.Millisecond)
	}()

	// --- Phase 2: Build address map and configure peer routing ---
	addrMap := make(map[string]string) // node ID → gRPC address
	for i, tn := range nodes {
		addrMap[ids[i]] = tn.node.GRPCAddr()
		t.Logf("node %s → %s", ids[i], tn.node.GRPCAddr())
	}

	// Tell each node how to reach its peers.
	for _, tn := range nodes {
		for id, addr := range addrMap {
			tn.node.SetPeerAddr(id, addr)
		}
	}

	// Start event loops AFTER peer addresses are wired.
	for i, tn := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		nodes[i].cancel = cancel
		go tn.node.Run(ctx)
	}

	// Wait for an election to complete.
	t.Log("waiting for leader election...")
	var leaderIdx int
	if !waitFor(t, 5*time.Second, func() bool {
		for i, tn := range nodes {
			if tn.node.Raft().State() == raft.Leader {
				leaderIdx = i
				return true
			}
		}
		return false
	}) {
		t.Fatal("no leader elected within 5 seconds")
	}
	t.Logf("leader elected: %s", ids[leaderIdx])

	// --- Phase 3: Send SET "foo"="bar" via Propose ---
	leaderAddr := addrMap[ids[leaderIdx]]
	cmd := fsm.Command{Op: fsm.OpSet, Key: "foo", Value: "bar"}

	// Use a client transport to send the Propose RPC.
	clientTransport, err := network.NewGRPCTransport(network.GRPCConfig{
		Addr:   "127.0.0.1:0",
		Logger: slog.Default().With("component", "client"),
	})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	defer clientTransport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := clientTransport.SendPropose(ctx, leaderAddr, &pb.ProposeRequest{
		Data: cmd.Encode(),
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !resp.Success {
		t.Fatalf("propose failed: %s (leader_addr=%s)", resp.Error, resp.LeaderAddr)
	}

	t.Log("proposal committed on leader")

	// --- Phase 4: Verify all nodes eventually have "bar" in their FSM ---
	t.Log("waiting for replication to all nodes...")
	if !waitFor(t, 5*time.Second, func() bool {
		for _, tn := range nodes {
			val, found := tn.node.FSM().Get("foo")
			if !found || val != "bar" {
				return false
			}
		}
		return true
	}) {
		// Print FSM state for debugging.
		for i, tn := range nodes {
			val, found := tn.node.FSM().Get("foo")
			t.Logf("  node-%d: found=%v val=%q state=%s commit=%d applied=%d",
				i, found, val,
				tn.node.Raft().State(),
				tn.node.Raft().CommitIndex(),
				tn.node.Raft().LastApplied(),
			)
		}
		t.Fatal("not all nodes replicated 'foo'='bar' within 5 seconds")
	}

	t.Log("✓ all 3 nodes have foo=bar in their KV state machines")
}

// waitFor polls predicate fn at 50ms intervals until it returns true
// or the timeout expires. Returns true on success.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if fn() {
				return true
			}
		}
	}
}
