package controlplane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "razpravljalnica/proto"

	"google.golang.org/protobuf/types/known/emptypb"
)

func newTestControlPlane() *ControlPlaneServer {
	mu := &sync.RWMutex{}
	state := &ControlPlaneState{
		Nodes:      make([]*pb.NodeInfo, 0),
		NextNodeId: 0,
	}
	return &ControlPlaneServer{
		mu:    mu,
		state: state,
		raft:  nil, // Tests run without Raft
	}
}

func TestRegisterNode(t *testing.T) {
	t.Skip("RegisterNode requires Raft leader, skipping for unit tests")
}

func TestRegisterMultipleNodes(t *testing.T) {
	t.Skip("RegisterNode requires Raft leader, skipping for unit tests")
}

func TestGetClusterStateEmpty(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	_, err := cp.GetClusterState(ctx, &emptypb.Empty{})
	//to se ne sme zgodit
	if err == nil {
		t.Error("Expected error when getting cluster state with no nodes, got nil")
	}
}

func TestGetClusterStateSingleNode(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Manually add a node to state for testing
	cp.mu.Lock()
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{
		NodeId:        "1",
		Address:       "localhost:5001",
		LastHeartbeat: time.Now().UnixNano(),
	})
	cp.mu.Unlock()

	// Get cluster state
	state, err := cp.GetClusterState(ctx, &emptypb.Empty{})

	if err != nil {
		t.Fatalf("GetClusterState failed: %v", err)
	}

	if state.Head == nil {
		t.Error("Expected head to be set, got nil")
	}

	if state.Tail == nil {
		t.Error("Expected tail to be set, got nil")
	}

	// With single node, head and tail should be the same
	if state.Head.Address != state.Tail.Address {
		t.Errorf("Expected head and tail to be same, got head=%s tail=%s",
			state.Head.Address, state.Tail.Address)
	}

	if len(state.Chain) != 1 {
		t.Errorf("Expected chain length 1, got %d", len(state.Chain))
	}
}

func TestGetClusterStateMultipleNodes(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Manually add multiple nodes to state for testing
	addresses := []string{"localhost:5001", "localhost:5002", "localhost:5003"}
	cp.mu.Lock()
	for i, addr := range addresses {
		cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{
			NodeId:        fmt.Sprintf("%d", i+1),
			Address:       addr,
			LastHeartbeat: time.Now().UnixNano(),
		})
	}
	cp.mu.Unlock()

	// Get cluster state
	state, err := cp.GetClusterState(ctx, &emptypb.Empty{})

	if err != nil {
		t.Fatalf("GetClusterState failed: %v", err)
	}

	// Check head
	if state.Head.Address != addresses[0] {
		t.Errorf("Expected head address '%s', got '%s'", addresses[0], state.Head.Address)
	}

	// Check tail
	if state.Tail.Address != addresses[len(addresses)-1] {
		t.Errorf("Expected tail address '%s', got '%s'",
			addresses[len(addresses)-1], state.Tail.Address)
	}

	// Check chain length
	if len(state.Chain) != len(addresses) {
		t.Errorf("Expected chain length %d, got %d", len(addresses), len(state.Chain))
	}

	// Verify chain structure
	for i, chainNode := range state.Chain {
		if chainNode.Info.Address != addresses[i] {
			t.Errorf("Chain node %d: expected address '%s', got '%s'",
				i, addresses[i], chainNode.Info.Address)
		}

		// Check prev pointer
		if i > 0 && chainNode.Prev != addresses[i-1] {
			t.Errorf("Chain node %d: expected prev '%s', got '%s'",
				i, addresses[i-1], chainNode.Prev)
		}

		// Check next pointer
		if i < len(addresses)-1 && chainNode.Next != addresses[i+1] {
			t.Errorf("Chain node %d: expected next '%s', got '%s'",
				i, addresses[i+1], chainNode.Next)
		}
	}
}

func TestGetSubscriptionNode(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Manually add nodes
	cp.mu.Lock()
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "1", Address: "localhost:5001", LastHeartbeat: time.Now().UnixNano()})
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "2", Address: "localhost:5002", LastHeartbeat: time.Now().UnixNano()})
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "3", Address: "localhost:5003", LastHeartbeat: time.Now().UnixNano()})
	cp.mu.Unlock()

	// Get subscription node for user
	resp, err := cp.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: 1})

	if err != nil {
		t.Fatalf("GetSubscriptionNode failed: %v", err)
	}

	if resp.Node == nil {
		t.Error("Expected node to be returned, got nil")
	}

	if resp.SubscribeToken == "" {
		t.Error("Expected subscription token, got empty string")
	}
}

func TestGetSubscriptionNodeNoNodes(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	_, err := cp.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: 1})

	if err == nil {
		t.Error("Expected error when getting subscription node with no nodes, got nil")
	}
}

func TestGetSubscriptionNodeDistribution(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Manually add 3 nodes
	cp.mu.Lock()
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "1", Address: "localhost:5001", LastHeartbeat: time.Now().UnixNano()})
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "2", Address: "localhost:5002", LastHeartbeat: time.Now().UnixNano()})
	cp.state.Nodes = append(cp.state.Nodes, &pb.NodeInfo{NodeId: "3", Address: "localhost:5003", LastHeartbeat: time.Now().UnixNano()})
	cp.mu.Unlock()

	// Test that different users get different nodes (modulo distribution)
	distribution := make(map[string]int)

	for userId := int64(1); userId <= 9; userId++ {
		resp, err := cp.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: userId})
		if err != nil {
			t.Fatalf("GetSubscriptionNode failed for user %d: %v", userId, err)
		}

		distribution[resp.Node.Address]++
	}

	// With 9 users and 3 nodes, each node should get exactly 3 users
	for addr, count := range distribution {
		if count != 3 {
			t.Errorf("Node %s: expected 3 users, got %d", addr, count)
		}
	}
}

// ne bom tega pisu je ok
func TestHeartbeat(t *testing.T) {
	t.Skip("Heartbeat requires Raft leader, skipping for unit tests")
}

func TestHeartbeatInvalidNode(t *testing.T) {
	t.Skip("Heartbeat requires Raft leader, skipping for unit tests")
}

func TestNodeRemovalAfterTTL(t *testing.T) {
	t.Skip("TTL loop requires Raft and is integration test, skipping for unit tests")
}

func TestHeartbeatResetsTimer(t *testing.T) {
	t.Skip("Heartbeat requires Raft leader, skipping for unit tests")
}

//tega je pisal vec chat ku jaz ka nism pisal jaz kode in sm ga pole sam popravlju ka pac je tko lepo napisal zakaj je nrdu v minuti kar sm jaz delal cel dan drgace
