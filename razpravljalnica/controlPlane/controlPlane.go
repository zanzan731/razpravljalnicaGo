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

	chain := make([]*pb.ChainNode, 0, len(c.nodes)) // to pomeni zcni seznam velikosti 0 ampak pre-allocate za len(c.nodes) sej lahk bi drugace ma to je lepo da lahk nucam append in da tud mu povem ej rezerviraj tolk placa da ne pole kake glupe nastanejo sicer ne bi smele ma ajde
	//go ma tko lepo clean sintaxo love go <3
	for i, n := range c.nodes {
		cn := &pb.ChainNode{Info: n}
		// prvi ne ma za druge mu dodaj prejsnjega
		if i > 0 {
			cn.Prev = c.nodes[i-1].Address
		}
		//zadnji ne ma za druge mu dodaj naslednjega
		if i < len(c.nodes)-1 {
			cn.Next = c.nodes[i+1].Address
		}
		chain = append(chain, cn)
	}

	return &pb.GetClusterStateResponse{
		Head:  c.nodes[0],              //prvi head
		Tail:  c.nodes[len(c.nodes)-1], //len vrne +1 ku pr c ka pac zcnes z 0 (ja sm falil na zacetku mb)
		Chain: chain,                   //vrni tud cel chain naj majo te podatke lih vsi zarad mene security ni pomembn zaenkrat sam da dela in tega tud ne mislim popravljat iskren
	}, nil
}

// h komu bo client subscribu
func (c *ControlPlaneServer) GetSubscriptionNode(ctx context.Context, req *pb.SubscriptionNodeRequest) (*pb.SubscriptionNodeResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	//ce ni nobenga serverja se client nima kam subscribat unlucky za njega kaj cmo
	if len(c.nodes) == 0 {
		return nil, fmt.Errorf("no nodes available")
	}

	//neki da je zaenkrat da porazdelimo userje po vozliscih ce ne stejemo da se tudi odjavijo bi blo popoln tud to
	//ubistvi bom lih tko pustu to mi je prov vsec ka vec je kompliciranje sploh za nas projekt
	idx := int(req.UserId) % len(c.nodes)
	node := c.nodes[idx]

	return &pb.SubscriptionNodeResponse{
		SubscribeToken: "OK", //lahk JWT pol (a se mi bo res dalo najbrz ne)
		Node:           node, //dej mu tistega ka smo ga gor izbrali
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
