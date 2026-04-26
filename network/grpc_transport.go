// grpc_transport.go implements the production gRPC transport for Phalanx.
//
// Architecture:
//   - JSON codec over gRPC unary RPCs (avoids protoc codegen dependency).
//   - Hand-written service descriptor matches proto/phalanx.proto exactly.
//   - IncomingRPC carries a response channel so the gRPC handler blocks
//     until the Node event loop processes the request via raft.Step().
//   - Client connections are lazy-initialized and cached per peer address.
//   - Latency interceptor logs consensus vs. client RPC durations.
//
// Default listen address: [::]:9000 (Fly.io IPv6 private mesh).
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"

	"github.com/basit-tijani/phalanx/pb"
)

// ---------------------------------------------------------------------------
// JSON Codec — gRPC uses this instead of protobuf for wire encoding.
// ---------------------------------------------------------------------------

const codecName = "json"

func init() {
	encoding.RegisterCodec(jsonCodec{})
}

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)     { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                      { return codecName }

// ---------------------------------------------------------------------------
// Incoming RPC Types
// ---------------------------------------------------------------------------

// IncomingRPC is the unit delivered to the Node event loop.
// Exactly one of AppendEntries / RequestVote is non-nil.
type IncomingRPC struct {
	AppendEntries *AppendEntriesRPC
	RequestVote   *RequestVoteRPC
}

// AppendEntriesRPC bundles an incoming AppendEntries call with
// a response channel. The gRPC handler blocks on Response until
// the Node writes the reply.
type AppendEntriesRPC struct {
	Request  *pb.AppendEntriesRequest
	Response chan *pb.AppendEntriesResponse
}

// RequestVoteRPC bundles an incoming RequestVote call with
// a response channel.
type RequestVoteRPC struct {
	Request  *pb.RequestVoteRequest
	Response chan *pb.RequestVoteResponse
}

// ---------------------------------------------------------------------------
// KV Client RPC Types (Phase 5)
// ---------------------------------------------------------------------------

// ProposeOp carries a client propose request into the Node event loop.
type ProposeOp struct {
	Request  *pb.ProposeRequest
	Response chan *pb.ProposeResponse
}

// ReadOp carries a client read request into the Node event loop.
type ReadOp struct {
	Request  *pb.ReadRequest
	Response chan *pb.ReadResponse
}

// ---------------------------------------------------------------------------
// gRPC Service Descriptor (hand-written, matches protoc output)
// ---------------------------------------------------------------------------

var consensusServiceDesc = grpc.ServiceDesc{
	ServiceName: "phalanx.Consensus",
	HandlerType: (*pb.ConsensusServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "AppendEntries", Handler: aeHandler},
		{MethodName: "RequestVote", Handler: rvHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "proto/phalanx.proto",
}

var kvServiceDesc = grpc.ServiceDesc{
	ServiceName: "phalanx.KV",
	HandlerType: (*pb.KVServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Propose", Handler: proposeHandler},
		{MethodName: "Read", Handler: readHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "proto/phalanx.proto",
}

func aeHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.AppendEntriesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(pb.ConsensusServer).AppendEntries(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/phalanx.Consensus/AppendEntries",
	}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(pb.ConsensusServer).AppendEntries(ctx, req.(*pb.AppendEntriesRequest))
	})
}

func rvHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.RequestVoteRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(pb.ConsensusServer).RequestVote(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/phalanx.Consensus/RequestVote",
	}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(pb.ConsensusServer).RequestVote(ctx, req.(*pb.RequestVoteRequest))
	})
}

func proposeHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.ProposeRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(pb.KVServer).Propose(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/phalanx.KV/Propose",
	}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(pb.KVServer).Propose(ctx, req.(*pb.ProposeRequest))
	})
}

func readHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.ReadRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(pb.KVServer).Read(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/phalanx.KV/Read",
	}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(pb.KVServer).Read(ctx, req.(*pb.ReadRequest))
	})
}

// ---------------------------------------------------------------------------
// Latency Interceptor — logs consensus vs. client RPC durations
// ---------------------------------------------------------------------------

func latencyInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)

		rpcType := "client"
		if strings.Contains(info.FullMethod, "AppendEntries") ||
			strings.Contains(info.FullMethod, "RequestVote") {
			rpcType = "consensus"
		}

		logger.Debug("rpc complete",
			"method", info.FullMethod,
			"type", rpcType,
			"latency_us", elapsed.Microseconds(),
		)
		return resp, err
	}
}

// ---------------------------------------------------------------------------
// GRPCTransport
// ---------------------------------------------------------------------------

// GRPCConfig defines parameters for the gRPC transport.
type GRPCConfig struct {
	Addr   string       // Listen address. Default: "[::]:9000".
	Logger *slog.Logger // Structured logger.
}

const defaultRPCBuffer = 256

// GRPCTransport implements production node-to-node communication over gRPC.
// Incoming RPCs are queued on a channel for the Node event loop.
// Outgoing RPCs use lazy-initialized, cached client connections.
type GRPCTransport struct {
	addr     string
	server   *grpc.Server
	listener net.Listener
	rpcs     chan IncomingRPC

	// KV client channels — read by the Node event loop.
	proposes chan ProposeOp
	reads    chan ReadOp

	// Client-side: lazy peer connections.
	mu    sync.RWMutex
	conns map[string]*grpc.ClientConn

	logger *slog.Logger
	closed atomic.Bool
}

// NewGRPCTransport creates a gRPC transport, starts the server, and
// begins accepting incoming RPCs on the specified address.
func NewGRPCTransport(cfg GRPCConfig) (*GRPCTransport, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = "[::]:9000" // Fly.io IPv6 default.
	}

	t := &GRPCTransport{
		addr:     addr,
		rpcs:     make(chan IncomingRPC, defaultRPCBuffer),
		proposes: make(chan ProposeOp, defaultRPCBuffer),
		reads:    make(chan ReadOp, defaultRPCBuffer),
		conns:    make(map[string]*grpc.ClientConn),
		logger:   cfg.Logger,
	}

	// Create server with latency interceptor.
	t.server = grpc.NewServer(
		grpc.UnaryInterceptor(latencyInterceptor(t.logger)),
	)

	// Register the consensus service using our hand-written descriptor.
	t.server.RegisterService(&consensusServiceDesc, &consensusGRPCHandler{rpcs: t.rpcs})

	// Register the KV client service.
	t.server.RegisterService(&kvServiceDesc, &kvGRPCHandler{proposes: t.proposes, reads: t.reads})

	// Bind listener.
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("grpc: listen %s: %w", addr, err)
	}
	t.listener = lis

	// Serve in background goroutine.
	go func() {
		if err := t.server.Serve(lis); err != nil && !t.closed.Load() {
			t.logger.Error("grpc server error", "err", err)
		}
	}()

	t.logger.Info("grpc transport started", "addr", lis.Addr().String())
	return t, nil
}

// RPCs returns the channel of incoming consensus RPCs.
// The Node event loop reads from this channel.
func (t *GRPCTransport) RPCs() <-chan IncomingRPC {
	return t.rpcs
}

// Proposes returns the channel of incoming Propose requests from clients.
func (t *GRPCTransport) Proposes() <-chan ProposeOp {
	return t.proposes
}

// Reads returns the channel of incoming Read requests from clients.
func (t *GRPCTransport) Reads() <-chan ReadOp {
	return t.reads
}

// Addr returns the actual listen address (useful when port 0 is used).
func (t *GRPCTransport) Addr() string {
	if t.listener != nil {
		return t.listener.Addr().String()
	}
	return t.addr
}

// getConn returns a cached gRPC client connection to the target,
// creating one on demand with double-checked locking.
func (t *GRPCTransport) getConn(target string) (*grpc.ClientConn, error) {
	t.mu.RLock()
	if conn, ok := t.conns[target]; ok {
		t.mu.RUnlock()
		return conn, nil
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	// Double-check after lock upgrade.
	if conn, ok := t.conns[target]; ok {
		return conn, nil
	}

	//nolint:staticcheck // grpc.Dial used for compatibility; upgrade to NewClient in Phase 5.
	conn, err := grpc.Dial(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype(codecName)),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc: dial %s: %w", target, err)
	}
	t.conns[target] = conn
	return conn, nil
}

// SendAppendEntries sends an AppendEntries RPC to the target peer.
// Blocks until the peer responds or the context expires.
func (t *GRPCTransport) SendAppendEntries(ctx context.Context, target string, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}
	out := new(pb.AppendEntriesResponse)
	err = conn.Invoke(ctx, "/phalanx.Consensus/AppendEntries", req, out)
	return out, err
}

// SendRequestVote sends a RequestVote RPC to the target peer.
// Blocks until the peer responds or the context expires.
func (t *GRPCTransport) SendRequestVote(ctx context.Context, target string, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}
	out := new(pb.RequestVoteResponse)
	err = conn.Invoke(ctx, "/phalanx.Consensus/RequestVote", req, out)
	return out, err
}

// Close gracefully stops the gRPC server and closes all client connections.
func (t *GRPCTransport) Close() error {
	if t.closed.Swap(true) {
		return nil // already closed
	}
	t.server.GracefulStop()

	t.mu.Lock()
	for addr, conn := range t.conns {
		conn.Close()
		delete(t.conns, addr)
	}
	t.mu.Unlock()

	close(t.rpcs)
	t.logger.Info("grpc transport shutdown")
	return nil
}

// ---------------------------------------------------------------------------
// Consensus gRPC Handler
// ---------------------------------------------------------------------------

// consensusGRPCHandler implements pb.ConsensusServer by queuing incoming
// RPCs on a buffered channel. The gRPC handler goroutine blocks until
// the Node event loop processes the request and writes a response.
type consensusGRPCHandler struct {
	rpcs chan<- IncomingRPC
}

func (h *consensusGRPCHandler) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	respCh := make(chan *pb.AppendEntriesResponse, 1)
	rpc := IncomingRPC{
		AppendEntries: &AppendEntriesRPC{Request: req, Response: respCh},
	}
	select {
	case h.rpcs <- rpc:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *consensusGRPCHandler) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	respCh := make(chan *pb.RequestVoteResponse, 1)
	rpc := IncomingRPC{
		RequestVote: &RequestVoteRPC{Request: req, Response: respCh},
	}
	select {
	case h.rpcs <- rpc:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// KV gRPC Handler
// ---------------------------------------------------------------------------

type kvGRPCHandler struct {
	proposes chan<- ProposeOp
	reads    chan<- ReadOp
}

func (h *kvGRPCHandler) Propose(ctx context.Context, req *pb.ProposeRequest) (*pb.ProposeResponse, error) {
	respCh := make(chan *pb.ProposeResponse, 1)
	op := ProposeOp{Request: req, Response: respCh}
	select {
	case h.proposes <- op:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *kvGRPCHandler) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	respCh := make(chan *pb.ReadResponse, 1)
	op := ReadOp{Request: req, Response: respCh}
	select {
	case h.reads <- op:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// KV Client Methods
// ---------------------------------------------------------------------------

// SendPropose sends a Propose RPC to the target node.
func (t *GRPCTransport) SendPropose(ctx context.Context, target string, req *pb.ProposeRequest) (*pb.ProposeResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}
	out := new(pb.ProposeResponse)
	err = conn.Invoke(ctx, "/phalanx.KV/Propose", req, out)
	return out, err
}

// SendRead sends a Read RPC to the target node.
func (t *GRPCTransport) SendRead(ctx context.Context, target string, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}
	out := new(pb.ReadResponse)
	err = conn.Invoke(ctx, "/phalanx.KV/Read", req, out)
	return out, err
}
