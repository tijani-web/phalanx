// Phalanx server binary — starts a consensus node.
//
// Configuration is via environment variables for container deployment:
//
//	NODE_ID    — unique node identifier (default: hostname)
//	PEERS      — comma-separated peer node IDs
//	DATA_DIR   — BadgerDB storage path (default: /data)
//	GRPC_ADDR  — gRPC listen address (default: [::]:9000)
//	DEBUG_ADDR — debug HTTP listen address (default: [::]:8080)
//	SEEDS      — comma-separated gossip seed addresses
//	BIND_ADDR  — gossip bind address (default: 0.0.0.0)
//	BIND_PORT  — gossip bind port (default: 7946)
//	TICK_MS    — tick interval in milliseconds (default: 200, tuned for global RTT)
//	ELECTION   — election timeout in ticks (default: 20, = 4s at 200ms ticks)
//	HEARTBEAT  — heartbeat timeout in ticks (default: 5, = 1s at 200ms ticks)
//
// Production defaults are tuned for a 5-node global mesh across
// JNB/LHR/ORD/SIN/FRA with cross-continental round-trip times of 100-300ms.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	phalanx "github.com/tijani-web/phalanx"
	"github.com/tijani-web/phalanx/discovery"
	"github.com/tijani-web/phalanx/logger"
)

func main() {
	nodeID := envOr("NODE_ID", hostname())
	peers := splitCSV(envOr("PEERS", ""))
	dataDir := envOr("DATA_DIR", "/data")
	grpcAddr := envOr("GRPC_ADDR", "[::]:9000")
	debugAddr := envOr("DEBUG_ADDR", "[::]:8080")
	seeds := splitCSV(envOr("SEEDS", ""))
	bindAddr := envOr("BIND_ADDR", "::")
	bindPort := envInt("BIND_PORT", 7946)
	tickMs := envInt("TICK_MS", 200)
	election := envInt("ELECTION", 20)
	heartbeat := envInt("HEARTBEAT", 5)

	// --- Logger ---
	term := &atomic.Uint64{}
	log := logger.New(nodeID, term)

	log.Info("starting phalanx",
		"node_id", nodeID,
		"peers", peers,
		"data_dir", dataDir,
		"grpc_addr", grpcAddr,
		"seeds", seeds,
	)

	// --- Node ---
	node, err := phalanx.NewNode(phalanx.NodeConfig{
		ID:               nodeID,
		Peers:            peers,
		TickInterval:     time.Duration(tickMs) * time.Millisecond,
		ElectionTimeout:  election,
		HeartbeatTimeout: heartbeat,
		DataDir:          dataDir,
		GRPCAddr:         grpcAddr,
		DebugAddr:        debugAddr,
		Logger:           log,
		Term:             term,
	})
	if err != nil {
		log.Error("failed to create node", "err", err)
		os.Exit(1)
	}

	// --- Discovery (optional) ---
	if len(seeds) > 0 {
		disc, err := discovery.New(discovery.Config{
			NodeID:   nodeID,
			BindAddr: bindAddr,
			BindPort: bindPort,
			RaftAddr: grpcAddr,
			Logger:   log,
		})
		if err != nil {
			log.Error("failed to start discovery", "err", err)
			os.Exit(1)
		}
		defer disc.Shutdown()

		if _, err := disc.Join(seeds); err != nil {
			log.Warn("seed join failed (will retry via gossip)", "err", err)
		}

		node.SetDiscovery(disc)
	}

	// --- Graceful Shutdown ---
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("node running — press Ctrl+C to stop")
	if err := node.Run(ctx); err != nil {
		log.Error("node exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("node stopped cleanly")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return fmt.Sprintf("node-%d", os.Getpid())
	}
	return h
}
