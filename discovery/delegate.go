package discovery

import (
	"github.com/hashicorp/memberlist"
)

// nodeDelegate implements memberlist.Delegate for metadata broadcasting.
// It serves a single purpose: make this node's RaftAddr available to
// every peer in the gossip mesh via NodeMeta.
type nodeDelegate struct {
	meta []byte
}

// NodeMeta returns the node's metadata for gossip broadcast.
// Called by memberlist when joining or responding to protocol messages.
func (d *nodeDelegate) NodeMeta(limit int) []byte {
	if len(d.meta) > limit {
		return d.meta[:limit]
	}
	return d.meta
}

// NotifyMsg is called when a user-data message is received.
// Not used in Phalanx — all communication goes through gRPC/Transport.
func (d *nodeDelegate) NotifyMsg([]byte) {}

// GetBroadcasts returns pending broadcasts. Not used — Phalanx
// does not piggyback data on gossip protocol messages.
func (d *nodeDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }

// LocalState returns local state for push/pull protocol.
func (d *nodeDelegate) LocalState(join bool) []byte { return nil }

// MergeRemoteState merges remote state from push/pull protocol.
func (d *nodeDelegate) MergeRemoteState(buf []byte, join bool) {}

// eventDelegate implements memberlist.EventDelegate for node join/leave detection.
// When memberlist detects a peer, the event is emitted on the channel for the
// Raft layer to consume and propose CONFIG_CHANGE entries.
type eventDelegate struct {
	events chan<- Event
}

// NotifyJoin is called when a node joins the gossip mesh.
// Extracts the RaftAddr from node metadata and emits a NodeJoin event.
func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	meta := DecodeMeta(node.Meta)
	select {
	case e.events <- Event{Type: NodeJoin, NodeID: node.Name, RaftAddr: meta.RaftAddr}:
	default:
		// Drop if consumer is not keeping up — bounded channel, non-blocking.
	}
}

// NotifyLeave is called when a node leaves or is detected as failed.
func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	meta := DecodeMeta(node.Meta)
	select {
	case e.events <- Event{Type: NodeLeave, NodeID: node.Name, RaftAddr: meta.RaftAddr}:
	default:
	}
}

// NotifyUpdate is called when a node's metadata is updated.
func (e *eventDelegate) NotifyUpdate(node *memberlist.Node) {
	meta := DecodeMeta(node.Meta)
	select {
	case e.events <- Event{Type: NodeUpdate, NodeID: node.Name, RaftAddr: meta.RaftAddr}:
	default:
	}
}
