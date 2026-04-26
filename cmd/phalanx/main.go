// Phalanx CLI — client tool for interacting with a Phalanx cluster.
//
// Commands:
//
//	phalanx put <key> <value> [-addr host:port]
//	phalanx get <key>         [-addr host:port]
//	phalanx status            [-addr host:port]
//
// Default address: localhost:9000 (gRPC) / localhost:8080 (debug HTTP).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"

	"github.com/basit-tijani/phalanx/fsm"
	"github.com/basit-tijani/phalanx/pb"
)

// ---------------------------------------------------------------------------
// JSON Codec (same as grpc_transport.go)
// ---------------------------------------------------------------------------

func init() {
	encoding.RegisterCodec(cliCodec{})
}

type cliCodec struct{}

func (cliCodec) Marshal(v any) ([]byte, error)     { return json.Marshal(v) }
func (cliCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (cliCodec) Name() string                      { return "json" }

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "put":
		doPut(args)
	case "get":
		doGet(args)
	case "status":
		doStatus(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Phalanx CLI — Consensus-driven KV Store

Usage:
  phalanx put <key> <value> [-addr host:port]   Write a key-value pair
  phalanx get <key>         [-addr host:port]   Read a value by key
  phalanx status            [-addr host:port]   Show cluster health

Flags:
  -addr   gRPC address for put/get (default: localhost:9000)
          HTTP address for status  (default: localhost:8080)`)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// Put Command
// ---------------------------------------------------------------------------

func doPut(args []string) {
	addr := "localhost:9000"
	filtered := parseAddr(args, &addr)

	if len(filtered) < 2 {
		fatal("usage: phalanx put <key> <value> [-addr host:port]")
	}
	key, value := filtered[0], filtered[1]

	conn := dial(addr)
	defer conn.Close()

	cmd := fsm.Command{Op: fsm.OpSet, Key: key, Value: value}
	req := &pb.ProposeRequest{Data: cmd.Encode()}
	resp := &pb.ProposeResponse{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := conn.Invoke(ctx, "/phalanx.KV/Propose", req, resp)
	if err != nil {
		fatal("rpc error: %v", err)
	}

	if resp.Success {
		fmt.Printf("✓ SET %s = %s\n", key, value)
	} else {
		fmt.Printf("✗ failed: %s", resp.Error)
		if resp.LeaderAddr != "" {
			fmt.Printf(" (try: -addr %s)", resp.LeaderAddr)
		}
		fmt.Println()
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Get Command
// ---------------------------------------------------------------------------

func doGet(args []string) {
	addr := "localhost:9000"
	filtered := parseAddr(args, &addr)

	if len(filtered) < 1 {
		fatal("usage: phalanx get <key> [-addr host:port]")
	}
	key := filtered[0]

	conn := dial(addr)
	defer conn.Close()

	req := &pb.ReadRequest{Key: key}
	resp := &pb.ReadResponse{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := conn.Invoke(ctx, "/phalanx.KV/Read", req, resp)
	if err != nil {
		fatal("rpc error: %v", err)
	}

	if resp.Error != "" {
		fmt.Printf("✗ %s", resp.Error)
		if resp.LeaderAddr != "" {
			fmt.Printf(" (try: -addr %s)", resp.LeaderAddr)
		}
		fmt.Println()
		os.Exit(1)
	}

	if resp.Found {
		fmt.Printf("%s = %s\n", key, resp.Value)
	} else {
		fmt.Printf("(not found) %s\n", key)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Status Command
// ---------------------------------------------------------------------------

func doStatus(args []string) {
	addr := "localhost:8080"
	parseAddr(args, &addr)

	// Ensure http:// prefix.
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}

	url := addr + "/debug/status"
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		fatal("http error: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		fatal("invalid JSON: %v", err)
	}

	// --- Pretty Table ---
	printHeader("NODE STATUS")
	printRow("Node ID", status["node_id"])
	printRow("State", status["state"])
	printRow("Term", status["term"])
	printRow("Leader", status["leader_id"])
	printRow("Commit Index", status["commit_index"])
	printRow("Applied Index", status["applied_index"])
	printRow("Log Length", status["log_length"])
	printRow("KV Size", status["kv_size"])

	if peers, ok := status["peers"].([]any); ok {
		peerStrs := make([]string, len(peers))
		for i, p := range peers {
			peerStrs[i] = fmt.Sprint(p)
		}
		printRow("Peers", strings.Join(peerStrs, ", "))
	}

	if metrics, ok := status["metrics"].(map[string]any); ok {
		fmt.Println()
		printHeader("METRICS")
		printRow("Messages Sent", metrics["messages_sent_total"])
		printRow("Elections", metrics["election_count"])
		printRow("Proposals", metrics["proposals_total"])
		printRow("Reads", metrics["reads_total"])
		printRow("State", metrics["current_state"])
	}

	if kvData, ok := status["kv_data"].(map[string]any); ok && len(kvData) > 0 {
		fmt.Println()
		printHeader("KV DATA")
		for k, v := range kvData {
			printRow(k, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func dial(addr string) *grpc.ClientConn {
	//nolint:staticcheck // grpc.Dial used for CLI simplicity.
	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype("json")),
	)
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return conn
}

func parseAddr(args []string, addr *string) []string {
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-addr" && i+1 < len(args) {
			*addr = args[i+1]
			i++ // skip next
		} else {
			filtered = append(filtered, args[i])
		}
	}
	return filtered
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func printHeader(title string) {
	line := strings.Repeat("─", 50)
	fmt.Printf("┌%s┐\n", line)
	pad := (50 - len(title)) / 2
	fmt.Printf("│%*s%s%*s│\n", pad, "", title, 50-pad-len(title), "")
	fmt.Printf("├%s┤\n", line)
}

func printRow(key string, val any) {
	fmt.Printf("│ %-20s %28v │\n", key, val)
}
