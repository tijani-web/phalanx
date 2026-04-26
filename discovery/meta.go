// Package discovery integrates HashiCorp memberlist (SWIM protocol)
// for zero-conf peer discovery in Phalanx clusters.
package discovery

// NodeMeta is the metadata each node broadcasts via the gossip mesh.
// Every node must advertise its RaftAddr so peers know where to
// send consensus RPCs (AppendEntries, RequestVote).
type NodeMeta struct {
	RaftAddr string
}

// Encode serializes the metadata for memberlist broadcast.
// Uses raw bytes — no encoding overhead. The RaftAddr is the only
// field and maps directly to the byte payload.
func (m NodeMeta) Encode() []byte {
	return []byte(m.RaftAddr)
}

// DecodeMeta deserializes node metadata from the gossip payload.
func DecodeMeta(data []byte) NodeMeta {
	return NodeMeta{RaftAddr: string(data)}
}
