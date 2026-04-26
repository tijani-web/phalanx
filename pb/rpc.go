package pb

import "context"

// ---------------------------------------------------------------------------
// Consensus Service (Phase 3)
// ---------------------------------------------------------------------------

// ConsensusServer is the server-side interface for the Consensus service.
// Equivalent to the protoc-generated ConsensusServer from phalanx.proto.
//
// Hand-written to avoid protoc toolchain dependency while maintaining
// the same RPC semantics. The gRPC service descriptor is defined in
// network/grpc_transport.go.
type ConsensusServer interface {
	AppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error)
	RequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error)
}

// ConsensusClient is the client-side interface for the Consensus service.
type ConsensusClient interface {
	AppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error)
	RequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error)
}

// ---------------------------------------------------------------------------
// KV Service (Phase 5)
// ---------------------------------------------------------------------------

// ProposeRequest carries a client mutation for Raft replication.
// Data is a JSON-encoded fsm.Command (SET or DELETE).
type ProposeRequest struct {
	Data []byte `json:"data"`
}

// ProposeResponse is the result of a Propose RPC.
// If the receiving node is not the leader, LeaderAddr tells the
// client where to redirect.
type ProposeResponse struct {
	Success    bool   `json:"success"`
	LeaderAddr string `json:"leader_addr,omitempty"` // For leader redirection.
	Error      string `json:"error,omitempty"`
}

// ReadRequest carries a key for linearizable reads.
type ReadRequest struct {
	Key string `json:"key"`
}

// ReadResponse returns the value for the requested key.
// If the receiving node cannot serve reads (not leader or lost quorum),
// LeaderAddr provides a redirect hint.
type ReadResponse struct {
	Value      string `json:"value"`
	Found      bool   `json:"found"`
	LeaderAddr string `json:"leader_addr,omitempty"`
	Error      string `json:"error,omitempty"`
}

// KVServer is the server-side interface for the client-facing KV service.
type KVServer interface {
	Propose(ctx context.Context, req *ProposeRequest) (*ProposeResponse, error)
	Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error)
}
