package main

import (
	"context"
	"fmt"
	"log"
	"net"
	pb "razpravljalnica/proto"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type ControlPlaneServer struct {
	pb.UnimplementedControlPlaneServer

	mu sync.RWMutex

	nodes      []*pb.NodeInfo
	nextNodeId int64
}

// trenutno je control plane samo en node,
// potem morajo še z raftom komunicirat

// vozlišče se prijavi in controlPlane ga shrani
func (c *ControlPlaneServer) RegisterNode(ctx context.Context, node *pb.NodeInfo) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextNodeId++
	node.NodeId = strconv.FormatInt(c.nextNodeId, 10) // control plane dodeli id
	// control plane bi lahko tudi dodelil address, lahko pa ga podamo kot parameter ko zaženemo node
	c.nodes = append(c.nodes, node)
	return &emptypb.Empty{}, nil
}

// vrne naslov trenutne glave in repa od serverja
func (c *ControlPlaneServer) GetClusterState(ctx context.Context, _ *emptypb.Empty) (*pb.GetClusterStateResponse, error) {
	if len(c.nodes) == 0 {
		return nil, fmt.Errorf("no nodes registered")
	}
	return &pb.GetClusterStateResponse{
		Head: c.nodes[0],
		Tail: c.nodes[len(c.nodes)-1],
	}, nil
}

func newServer() *ControlPlaneServer {
	return &ControlPlaneServer{}
}

// server nodi se bodo registrirali
// server nodi pošiljajo heartbeat de vemo de so še živi
// client bo lohk od control planea zahteval naslov glave in repa (kliče v intervalih
// ali kadar je kak error)
func main() {
	lis, err := net.Listen("tcp", ":6000")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterControlPlaneServer(grpcServer, newServer())

	log.Println("ControlPlane listening on :6000")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
