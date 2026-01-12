package controlplane

//se je preimenovalo ker je case sensitive ocitn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	pb "razpravljalnica/proto"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

type ControlPlaneState struct {
	Nodes      []*pb.NodeInfo
	NextNodeId int64
}

// state se spreminja samo preko rafta
type ControlPlaneServer struct {
	pb.UnimplementedControlPlaneServer

	mu    *sync.RWMutex
	raft  *raft.Raft
	state *ControlPlaneState
}

type ControlPlaneFSM struct {
	mu    *sync.RWMutex
	state *ControlPlaneState
}

type CommandType string

const (
	CmdRegisterNode CommandType = "register_node"
	CmdRemoveNode   CommandType = "remove_node"
	CmdHeartbeat    CommandType = "heartbeat"
)

type RaftCommand struct {
	Type CommandType
	Node pb.NodeInfo
}

const nodeTTL = 10 * time.Second

// vrže error, če ta node ni leader
func (c *ControlPlaneServer) ensureLeader() error {
	if c.raft.State() != raft.Leader {
		return fmt.Errorf("not leader")
	}
	return nil
}

// vozlišče(server.go) se prijavi in controlPlane ga shrani
func (c *ControlPlaneServer) RegisterNode(ctx context.Context, node *pb.NodeInfo) (*pb.RegisterNodeResponse, error) {
	if err := c.ensureLeader(); err != nil {
		return nil, err
	}
	// samo leader lahko registrira node, če ni leader vrne error

	cmd := RaftCommand{
		Type: CmdRegisterNode,
		Node: *node,
	}

	data, _ := json.Marshal(cmd)
	f := c.raft.Apply(data, 5*time.Second) // izvede command (v tem primeru register node)
	if err := f.Error(); err != nil {
		return nil, err
	}

	nodeId := f.Response().(string)
	return &pb.RegisterNodeResponse{NodeId: nodeId}, nil
}

// vrne naslov trenutne glave in repa od serverja
func (c *ControlPlaneServer) GetClusterState(ctx context.Context, _ *emptypb.Empty) (*pb.GetClusterStateResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.state.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes registered")
	}

	chain := make([]*pb.ChainNode, 0, len(c.state.Nodes))

	for i, n := range c.state.Nodes {
		cn := &pb.ChainNode{Info: n}
		if i > 0 {
			cn.Prev = c.state.Nodes[i-1].Address
		}
		if i < len(c.state.Nodes)-1 {
			cn.Next = c.state.Nodes[i+1].Address
		}
		chain = append(chain, cn)
	}

	return &pb.GetClusterStateResponse{
		Head:  c.state.Nodes[0],
		Tail:  c.state.Nodes[len(c.state.Nodes)-1],
		Chain: chain,
	}, nil
}

// h komu bo client subscribu
func (c *ControlPlaneServer) GetSubscriptionNode(ctx context.Context, req *pb.SubscriptionNodeRequest) (*pb.SubscriptionNodeResponse, error) {

	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.state.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes available")
	}

	idx := int(req.UserId) % len(c.state.Nodes)
	return &pb.SubscriptionNodeResponse{
		SubscribeToken: "OK",
		Node:           c.state.Nodes[idx],
	}, nil

}

func (c *ControlPlaneServer) Heartbeat(ctx context.Context, node *pb.NodeInfo) (*emptypb.Empty, error) {
	if err := c.ensureLeader(); err != nil {
		return nil, err
	}

	cmd := RaftCommand{
		Type: CmdHeartbeat,
		Node: pb.NodeInfo{
			NodeId:        node.NodeId,
			LastHeartbeat: time.Now().UnixNano(),
		},
	}

	data, _ := json.Marshal(cmd)
	f := c.raft.Apply(data, 5*time.Second)
	return &emptypb.Empty{}, f.Error()
}

func (c *ControlPlaneServer) startTTLLoop() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for range ticker.C {
			if c.raft.State() != raft.Leader {
				continue
			}

			c.mu.RLock()
			nodes := append([]*pb.NodeInfo(nil), c.state.Nodes...)
			c.mu.RUnlock()
			now := time.Now().UnixNano()
			for _, n := range nodes {
				if now-n.LastHeartbeat > int64(nodeTTL) {
					cmd := RaftCommand{
						Type: CmdRemoveNode,
						Node: pb.NodeInfo{NodeId: n.NodeId},
					}
					data, _ := json.Marshal(cmd)
					c.raft.Apply(data, 5*time.Second)
				}
			}
		}
	}()
}

func (c *ControlPlaneServer) JoinCluster(ctx context.Context, req *pb.JoinClusterRequest) (*pb.JoinClusterResponse, error) {
	// če ni leader mu pošlje leaderjev naslov
	if c.raft.State() != raft.Leader {
		leaderRaftAddr := c.raft.Leader()
		if leaderRaftAddr == "" {
			return nil, fmt.Errorf("no leader elected yet")
		}

		leaderGrpcAddr := raftAddrToGrpcAddr(string(leaderRaftAddr))

		return &pb.JoinClusterResponse{
			LeaderGrpcAddr: leaderGrpcAddr,
		}, nil
	}

	// preveri, če je že voter
	f := c.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}

	for _, srv := range f.Configuration().Servers {
		if srv.ID == raft.ServerID(req.NodeId) {
			log.Printf("Node %s already member", req.NodeId)
			return &pb.JoinClusterResponse{}, nil
		}
	}

	// add voter
	add := c.raft.AddVoter(
		raft.ServerID(req.NodeId),
		raft.ServerAddress(req.RaftAddr),
		0,
		10*time.Second,
	)
	if err := add.Error(); err != nil {
		return nil, err
	}

	log.Printf("Raft node joined: id=%s addr=%s", req.NodeId, req.RaftAddr)
	return &pb.JoinClusterResponse{}, nil
}

func (c *ControlPlaneServer) GetLeaderAddr(ctx context.Context, _ *emptypb.Empty) (*pb.LeaderAddressResponse, error) {
	addr, _ := c.raft.LeaderWithID()
	if addr == "" {
		return &pb.LeaderAddressResponse{
			LeaderAddr: "",
		}, nil
	}
	var saddr string = raftAddrToGrpcAddr(string(addr))
	return &pb.LeaderAddressResponse{
		LeaderAddr: saddr,
	}, nil
}

func (f *ControlPlaneFSM) Apply(log *raft.Log) interface{} {
	var cmd RaftCommand
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		panic(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch cmd.Type {

	case CmdRegisterNode:
		f.state.NextNodeId++
		cmd.Node.NodeId = strconv.FormatInt(f.state.NextNodeId, 10)
		cmd.Node.LastHeartbeat = time.Now().UnixNano()
		f.state.Nodes = append(f.state.Nodes, &cmd.Node)
		return cmd.Node.NodeId

	case CmdHeartbeat:
		for _, n := range f.state.Nodes {
			if n.NodeId == cmd.Node.NodeId {
				n.LastHeartbeat = cmd.Node.LastHeartbeat
			}
		}

	case CmdRemoveNode:
		for i, n := range f.state.Nodes {
			if n.NodeId == cmd.Node.NodeId {
				f.state.Nodes = append(f.state.Nodes[:i], f.state.Nodes[i+1:]...)
				break
			}
		}
	}

	return nil
}

func (f *ControlPlaneFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, _ := json.Marshal(f.state)
	return &fsmSnapshot{data}, nil
}

func (f *ControlPlaneFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return json.NewDecoder(rc).Decode(f.state)
}

type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// server nodi se bodo registrirali
// server nodi pošiljajo heartbeat de vemo de so še živi
// client bo lohk od control planea zahteval naslov glave in repa (kliče v intervalih
// ali kadar je kak error)
func Run(grpcAddr string, raftAddr string, nodeID string, bootstrap bool, leaderGrpcAddr string) {
	//meni tko lepsi krajsi commandi
	raftAddr = "localhost:" + raftAddr
	grpcAddr = "localhost:" + grpcAddr
	leaderGrpcAddr = "localhost:" + leaderGrpcAddr
	mu := &sync.RWMutex{}
	state := &ControlPlaneState{}
	fsm := &ControlPlaneFSM{state: state, mu: mu}

	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(nodeID)

	logStore, _ := raftboltdb.NewBoltStore(fmt.Sprintf("raft-%s-log.bolt", nodeID))
	stableStore, _ := raftboltdb.NewBoltStore(fmt.Sprintf("raft-%s-stable.bolt", nodeID))
	snapStore, _ := raft.NewFileSnapshotStore(fmt.Sprintf("snapshots-%s", nodeID), 1, os.Stdout)

	transport, _ := raft.NewTCPTransport(raftAddr, nil, 3, 10*time.Second, os.Stdout)

	hasState, err := raft.HasExistingState(logStore, stableStore, snapStore)
	if err != nil {
		log.Fatal(err)
	}
	r, err := raft.NewRaft(config, fsm, logStore, stableStore, snapStore, transport)
	if err != nil {
		log.Fatal(err)
	}

	// bootstrap mora biti točno eden in ta je prvi leader
	if bootstrap && !hasState { // hasState preveri, če je server vmes padu in se spret konenkta, kar bi pokvarlo podatke
		r.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{{
				ID:      config.LocalID,
				Address: transport.LocalAddr(),
			}},
		})
	}
	if !bootstrap && !hasState {
		go JoinRaftCluster(leaderGrpcAddr, nodeID, raftAddr)
	}

	server := &ControlPlaneServer{
		raft:  r,
		state: state,
		mu:    mu,
	}
	server.startTTLLoop()
	lis, _ := net.Listen("tcp", grpcAddr)
	grpcServer := grpc.NewServer()
	pb.RegisterControlPlaneServer(grpcServer, server)

	log.Println("ControlPlane listening on", grpcAddr)
	grpcServer.Serve(lis)
}

func JoinRaftCluster(initialGrpcAddr string, nodeID string, raftAddr string) error {
	// to pokličejo tisti nodi, ki niso bootstrap, da se joinajo leaderju
	target := initialGrpcAddr
	for {
		conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return err
		}

		client := pb.NewControlPlaneClient(conn)
		resp, err := client.JoinCluster(context.Background(), &pb.JoinClusterRequest{
			NodeId:   nodeID,
			RaftAddr: raftAddr,
		})

		conn.Close()
		// ponavljamo, dokler se nam ne rata joinat
		if err != nil {
			log.Println("Join failed, retrying:", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// "" pomeni, da še ni leaderja, torej se ne poskušamo več joinat
		if resp.LeaderGrpcAddr == "" {
			log.Println("Successfully joined Raft cluster")
			return nil
		}

		log.Println("Redirected to leader:", resp.LeaderGrpcAddr)
		target = resp.LeaderGrpcAddr
	}
}

func raftAddrToGrpcAddr(raftAddr string) string {
	if raftAddr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(raftAddr)
	if err != nil {
		log.Printf("Error parsing raft address %s: %v", raftAddr, err)
		return ""
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		log.Printf("Error parsing port %s: %v", port, err)
		return ""
	}
	// za zdaj ja grpc address ravno raft address-1000 (bi blo fajn mal spremenit)
	return fmt.Sprintf("%s:%d", host, p-1000)
}
