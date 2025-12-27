package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "razpravljalnica/proto"

	"google.golang.org/protobuf/types/known/emptypb"
)

func newTestControlPlane() *ControlPlaneServer {
	return &ControlPlaneServer{
		nodes:      make([]*NodeTTL, 0),
		nextNodeId: 0,
	}
}

func TestRegisterNode(t *testing.T) {
	cp := newTestControlPlane()

	node := &pb.NodeInfo{
		NodeId:  fmt.Sprint(cp.nextNodeId),
		Address: "localhost:5001",
	}

	resp, err := cp.RegisterNode(context.Background(), node)

	if err != nil {
		t.Fatalf("RegisterNode failed: %v", err)
	}

	if resp.NodeId == "" {
		t.Error("Expected node ID to be assigned, got empty string")
	}

	// Verify node is stored
	cp.mu.RLock()
	nodeCount := len(cp.nodes)
	storedNode := cp.nodes[0]
	cp.mu.RUnlock()

	if nodeCount != 1 {
		t.Errorf("Expected 1 node, got %d", nodeCount)
	}

	if storedNode.node.Address != "localhost:5001" {
		t.Errorf("Expected address 'localhost:5001', got '%s'", storedNode.node.Address)
	}
}

func TestRegisterMultipleNodes(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	addresses := []string{"localhost:5001", "localhost:5002", "localhost:5003"}

	for _, addr := range addresses {
		node := &pb.NodeInfo{Address: addr}
		_, err := cp.RegisterNode(ctx, node)

		if err != nil {
			t.Fatalf("RegisterNode failed for %s: %v", addr, err)
		}
	}

	cp.mu.RLock()
	nodeCount := len(cp.nodes)
	cp.mu.RUnlock()

	if nodeCount != len(addresses) {
		t.Errorf("Expected %d nodes, got %d", len(addresses), nodeCount)
	}
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

	// Register a node
	node := &pb.NodeInfo{Address: "localhost:5001"}
	cp.RegisterNode(ctx, node)

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

	// Register multiple nodes
	addresses := []string{"localhost:5001", "localhost:5002", "localhost:5003"}
	for _, addr := range addresses {
		cp.RegisterNode(ctx, &pb.NodeInfo{Address: addr})
	}

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

	// Register nodes
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5001"})
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5002"})
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5003"})

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

	// Register 3 nodes
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5001"})
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5002"})
	cp.RegisterNode(ctx, &pb.NodeInfo{Address: "localhost:5003"})

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

func TestHeartbeat(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Register a node
	node := &pb.NodeInfo{Address: "localhost:5001"}
	resp, _ := cp.RegisterNode(ctx, node)

	// Send heartbeat
	_, err := cp.Heartbeat(ctx, &pb.NodeInfo{NodeId: resp.NodeId, Address: "localhost:5001"})

	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
}

func TestHeartbeatInvalidNode(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Try to send heartbeat for non-existent node
	_, err := cp.Heartbeat(ctx, &pb.NodeInfo{NodeId: "2323131321", Address: "localhost:5001"})

	if err == nil {
		t.Error("Expected error for heartbeat with invalid node ID, got nil")
	}
}

func TestNodeRemovalAfterTTL(t *testing.T) {
	// This test is tricky because it involves timing
	// We'll create a control plane with a very short TTL for testing
	cp := newTestControlPlane()
	ctx := context.Background()

	// Register a node
	node := &pb.NodeInfo{Address: "localhost:5001"}
	cp.RegisterNode(ctx, node)

	// Verify node exists
	cp.mu.RLock()
	initialCount := len(cp.nodes)
	cp.mu.RUnlock()

	if initialCount != 1 {
		t.Errorf("Expected 1 node initially, got %d", initialCount)
	}

	// Wait for TTL to expire (plus a bit more)
	// Note: nodeTTL is 10 seconds, so this test will take 10+ seconds
	// For real testing, you might want to make TTL configurable
	time.Sleep(11 * time.Second)

	// Check if node was removed
	cp.mu.RLock()
	finalCount := len(cp.nodes)
	cp.mu.RUnlock()

	if finalCount != 0 {
		t.Errorf("Expected node to be removed after TTL, got %d nodes", finalCount)
	}
}

func TestHeartbeatResetsTimer(t *testing.T) {
	cp := newTestControlPlane()
	ctx := context.Background()

	// Register a node
	node := &pb.NodeInfo{Address: "localhost:5001"}
	resp, _ := cp.RegisterNode(ctx, node)

	// Wait half the TTL
	time.Sleep(5 * time.Second)

	// Send heartbeat to reset timer
	cp.Heartbeat(ctx, &pb.NodeInfo{NodeId: resp.NodeId, Address: "localhost:5001"})

	// Wait another half TTL (total: 10 seconds since registration, 5 since heartbeat)
	time.Sleep(5 * time.Second)

	// Node should still exist because heartbeat reset the timer
	cp.mu.RLock()
	nodeCount := len(cp.nodes)
	cp.mu.RUnlock()

	if nodeCount != 1 {
		t.Errorf("Expected node to still exist after heartbeat, got %d nodes", nodeCount)
	}
}

//tega je pisal vec chat ku jaz ka nism pisal jaz kode in sm ga pole sam popravlju ka pac je tko lepo napisal zakaj je nrdu v minuti kar sm jaz delal cel dan drgace
