package discovery

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basit-tijani/phalanx/logger"
	"github.com/basit-tijani/phalanx/network"
)

// freePort asks the OS for an available TCP port on loopback.
// The TOCTOU race is acceptable in tests — ports are ephemeral.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// TestDiscovery_ThreeNodeMesh starts 3 nodes, joins them via gossip,
// auto-connects their LocalTransports based on discovered metadata,
// and proves cross-mesh communication works end-to-end.
func TestDiscovery_ThreeNodeMesh(t *testing.T) {
	const nodeCount = 3

	// Allocate free ports and raft addresses.
	ports := make([]int, nodeCount)
	raftAddrs := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		ports[i] = freePort(t)
		raftAddrs[i] = fmt.Sprintf("raft-node-%d", i+1)
	}

	// Create LocalTransports keyed by raft address.
	transports := make(map[string]*network.LocalTransport, nodeCount)
	for _, addr := range raftAddrs {
		transports[addr] = network.NewLocalTransport(addr)
	}
	defer func() {
		for _, tr := range transports {
			tr.Close()
		}
	}()

	// Create Discovery Managers.
	term := &atomic.Uint64{}
	managers := make([]*Manager, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodeID := fmt.Sprintf("node-%d", i+1)
		log := logger.New(nodeID, term)

		m, err := New(Config{
			NodeID:   nodeID,
			BindAddr: "127.0.0.1",
			BindPort: ports[i],
			RaftAddr: raftAddrs[i],
			Logger:   log,
		})
		if err != nil {
			t.Fatalf("failed to create manager for %s: %v", nodeID, err)
		}
		managers[i] = m
	}
	defer func() {
		for _, m := range managers {
			m.Shutdown()
		}
	}()

	// Join nodes 1 and 2 to node 0's gossip address.
	seed := managers[0].MemberAddr()
	for i := 1; i < nodeCount; i++ {
		if _, err := managers[i].Join([]string{seed}); err != nil {
			t.Fatalf("node-%d join failed: %v", i+1, err)
		}
	}

	// Process events: auto-connect transports based on gossip discovery.
	// Each node should discover the other 2 peers.
	connected := make([]map[string]bool, nodeCount)
	for i := 0; i < nodeCount; i++ {
		connected[i] = make(map[string]bool)
	}

	allConnected := func() bool {
		for _, peers := range connected {
			if len(peers) < nodeCount-1 {
				return false
			}
		}
		return true
	}

	deadline := time.After(10 * time.Second)
	for !allConnected() {
		for i, m := range managers {
			select {
			case ev := <-m.Events():
				// Skip self-join events.
				if ev.NodeID == m.config.NodeID {
					continue
				}
				if ev.Type == NodeJoin && !connected[i][ev.RaftAddr] {
					// Bridge: gossip discovery → transport connection.
					myAddr := raftAddrs[i]
					network.Connect(transports[myAddr], transports[ev.RaftAddr])
					connected[i][ev.RaftAddr] = true

					t.Logf("[%s] discovered %s at %s", m.config.NodeID, ev.NodeID, ev.RaftAddr)
				}
			default:
			}
		}

		select {
		case <-deadline:
			for i, peers := range connected {
				t.Logf("node-%d connected to %d peers", i+1, len(peers))
			}
			t.Fatal("timed out waiting for full mesh discovery")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Verify: node-1 can send to node-3 through the auto-connected mesh.
	entry := network.AcquireEntry()
	entry.Index = 1
	entry.Term = 1
	entry.Data = make([]byte, 4)
	copy(entry.Data, []byte("ping"))
	defer network.ReleaseEntry(entry)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := transports["raft-node-1"].Send(ctx, "raft-node-3", entry); err != nil {
		t.Fatalf("cross-mesh send (node-1 → node-3) failed: %v", err)
	}

	select {
	case msg := <-transports["raft-node-3"].Consume():
		if string(msg.Entry.Data) != "ping" {
			t.Errorf("Data: want 'ping', got '%s'", msg.Entry.Data)
		}
		if msg.From != "raft-node-1" {
			t.Errorf("From: want 'raft-node-1', got '%s'", msg.From)
		}
		network.ReleaseEntry(msg.Entry)
		t.Log("cross-mesh communication verified: node-1 → node-3")
	case <-ctx.Done():
		t.Fatal("cross-mesh receive timed out")
	}

	// Verify all 3 nodes see each other in memberlist.
	for i, m := range managers {
		members := m.Members()
		if len(members) != nodeCount {
			t.Errorf("node-%d sees %d members, want %d", i+1, len(members), nodeCount)
		}
	}
}

// TestDiscovery_EventTypes verifies that Join and Leave events are emitted correctly.
func TestDiscovery_EventTypes(t *testing.T) {
	port1 := freePort(t)
	port2 := freePort(t)

	term := &atomic.Uint64{}
	log1 := logger.New("node-1", term)
	log2 := logger.New("node-2", term)

	m1, err := New(Config{
		NodeID:   "node-1",
		BindAddr: "127.0.0.1",
		BindPort: port1,
		RaftAddr: "raft-1",
		Logger:   log1,
	})
	if err != nil {
		t.Fatalf("m1 create failed: %v", err)
	}
	defer m1.Shutdown()

	m2, err := New(Config{
		NodeID:   "node-2",
		BindAddr: "127.0.0.1",
		BindPort: port2,
		RaftAddr: "raft-2",
		Logger:   log2,
	})
	if err != nil {
		t.Fatalf("m2 create failed: %v", err)
	}

	// Join m2 to m1.
	if _, err := m2.Join([]string{m1.MemberAddr()}); err != nil {
		t.Fatalf("join failed: %v", err)
	}

	// Drain m1's events — expect a JOIN for node-2.
	timeout := time.After(5 * time.Second)
	var gotJoin bool
	for !gotJoin {
		select {
		case ev := <-m1.Events():
			if ev.Type == NodeJoin && ev.NodeID == "node-2" {
				if ev.RaftAddr != "raft-2" {
					t.Errorf("RaftAddr: want 'raft-2', got '%s'", ev.RaftAddr)
				}
				gotJoin = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for JOIN event on m1")
		}
	}

	// Shutdown m2 — m1 should get a LEAVE event.
	m2.Shutdown()

	var gotLeave bool
	timeout = time.After(10 * time.Second)
	for !gotLeave {
		select {
		case ev := <-m1.Events():
			if ev.Type == NodeLeave && ev.NodeID == "node-2" {
				gotLeave = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for LEAVE event on m1")
		}
	}
}

// TestDiscovery_NoGoroutineLeak verifies that shutting down the discovery
// manager does not leak goroutines.
func TestDiscovery_NoGoroutineLeak(t *testing.T) {
	// Warm up runtime and let background goroutines settle.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()

	port := freePort(t)
	term := &atomic.Uint64{}
	log := logger.New("leak-test", term)

	m, err := New(Config{
		NodeID:   "leak-test",
		BindAddr: "127.0.0.1",
		BindPort: port,
		RaftAddr: "raft-leak",
		Logger:   log,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Drain any self-join events.
	time.Sleep(200 * time.Millisecond)
	for {
		select {
		case <-m.Events():
		default:
			goto drained
		}
	}
drained:

	m.Shutdown()

	// Give memberlist's internal goroutines time to wind down.
	time.Sleep(2 * time.Second)
	runtime.GC()

	after := runtime.NumGoroutine()
	// Allow slack of 2 for runtime-internal goroutines (GC, finalizer, etc.).
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d, after=%d (delta=%d)", before, after, after-before)
	}
}

// TestDiscovery_MetadataRoundTrip verifies node metadata encodes and
// decodes correctly through the gossip mesh.
func TestDiscovery_MetadataRoundTrip(t *testing.T) {
	port1 := freePort(t)
	port2 := freePort(t)

	term := &atomic.Uint64{}

	// Node 1 advertises an IPv6 raft address — Fly.io scenario.
	raftAddr1 := "[fdaa:0:1::1]:9000"
	m1, err := New(Config{
		NodeID:   "node-1",
		BindAddr: "127.0.0.1",
		BindPort: port1,
		RaftAddr: raftAddr1,
		Logger:   logger.New("node-1", term),
	})
	if err != nil {
		t.Fatalf("m1 create failed: %v", err)
	}
	defer m1.Shutdown()

	m2, err := New(Config{
		NodeID:   "node-2",
		BindAddr: "127.0.0.1",
		BindPort: port2,
		RaftAddr: "raft-2",
		Logger:   logger.New("node-2", term),
	})
	if err != nil {
		t.Fatalf("m2 create failed: %v", err)
	}
	defer m2.Shutdown()

	if _, err := m2.Join([]string{m1.MemberAddr()}); err != nil {
		t.Fatalf("join failed: %v", err)
	}

	// m2 should receive a JOIN event with m1's IPv6 raft address.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-m2.Events():
			if ev.Type == NodeJoin && ev.NodeID == "node-1" {
				if ev.RaftAddr != raftAddr1 {
					t.Errorf("RaftAddr roundtrip failed: want '%s', got '%s'", raftAddr1, ev.RaftAddr)
				} else {
					t.Logf("metadata roundtrip OK: %s → %s", ev.NodeID, ev.RaftAddr)
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for JOIN event with metadata")
		}
	}
}
