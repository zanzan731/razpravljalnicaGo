package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	pb "razpravljalnica/proto"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

/*
rabim strukt za server, kaj rabim hraniti?
-uporabnike
-teme
-sporočila
-ID-je
....
*/
type messageBoardServer struct {
	//to ti da default vseh funkcij kar rabis go pol sam ustvari
	pb.UnimplementedMessageBoardServer

	mu       sync.RWMutex // protect access
	nodeInfo *pb.NodeInfo // vsi podatki tega noda
	isHead   bool         //je head?
	isTail   bool         //je tail?

	controlPlaneAddr string //kje je control plane
	nextUserID       int64  //globalen id za userje da nastavim naslednjemu
	nextTopicID      int64  //ist za topic
	nextMsgID        int64  //ist za msg
	//v go je edina omejitev za dolzino tvoj spomin tko da chillamo
	users    map[int64]*pb.User
	topics   map[int64]*pb.Topic
	messages map[int64][]*pb.Message

	subscribers map[int64][]pb.MessageBoard_SubscribeTopicServer // topicID → streams

	// zacetek replikacije dej prejsni server in naslednji
	nextClient pb.MessageBoardClient
	nextConn   *grpc.ClientConn
	nextAddr   string
	nextDialMu sync.Mutex
}

/*
ne rabimo vec narejeno na control plane --zbrisi naslednjic ce vse dela ce ne pusti za testing

	func newServer() *messageBoardServer {
		return &messageBoardServer{
			users:       make(map[int64]*pb.User),
			topics:      make(map[int64]*pb.Topic),
			messages:    make(map[int64][]*pb.Message),
			subscribers: make(map[int64][]pb.MessageBoard_SubscribeTopicServer),
		}
	}
*/
func (s *messageBoardServer) printAll() {
	fmt.Println("Users: ")
	for _, u := range s.users {
		fmt.Println(u.Id, u.Name)
	}
	fmt.Println("Topics: ")
	for _, u := range s.topics {
		fmt.Println(u.Id, u.Name)
	}
	fmt.Println("Messages: ")
	for _, t := range s.topics {
		for _, u := range s.messages[t.Id] {
			fmt.Println(u.Text)
		}
	}
}

// checkIsHeadNow prasa v control plane kdo je head zato ka za nek razlog ce zelo hitro pozenem 2 mi lahko se zmedejo nodi tko da + lahk da head gre dol pole treba mal sprement
func (s *messageBoardServer) checkIsHeadNow() bool {
	// ne zelim shranjevat connectiona zato oprem en connection ki ga pole zaprem sam za to je chat pomagu pa reku da je to ok tko da recmo jaz nism preprican
	conn, err := grpc.NewClient(s.controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		// Ce slucajn control plane ni vec up zapi tistemu kar mam tle cached... zaenkrat tko pole mogoc za zamenjat ma tud ce ne
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		log.Printf("Control plane did not respond, using cached isHead=%v for %s", isHead, s.nodeInfo.GetAddress())
		return isHead
	}
	defer conn.Close() //ce je connection se vzpostavu ga na konci zbris

	cp := pb.NewControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	//dubi podatke s controla
	state, err := cp.GetClusterState(ctx, &emptypb.Empty{})
	cancel()
	if err != nil {
		// ce faila zaupi temu kar mas
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		log.Printf("GetClusterState failed: %v, using cached isHead=%v for %s", err, isHead, s.nodeInfo.GetAddress())
		return isHead
	}

	isHeadNow := state.Head.Address == s.nodeInfo.GetAddress() //ce smo mi true drgac pac false
	log.Printf("GetClusterState returned head without errors=%s, I am %s, so isHead=%v", state.Head.Address, s.nodeInfo.GetAddress(), isHeadNow)
	return isHeadNow
}

// ali je iy serverja ali clienta check zato ka rabim vedet da se je na head v prvo poslalo in pole po tem pac dodas en podpis da ves da je ok - chatko pomagu
func isInternalCall(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	vals := md.Get("x-internal-replication")
	return len(vals) > 0 && vals[0] == "true"
}

// replicate gre po chainu in replicata podatke tail neha replicatat ka nima vec naslednjika to je sam helper dejsnski replicate se izvaja zdravn zapisov
func (s *messageBoardServer) replicate(fn func(ctx context.Context) error) error {
	// dubi naslednjika ce ne ves za njegov obstoj
	if err := s.ensureNextClient(); err != nil {
		return fmt.Errorf("replication failed (next unavailable): %w", err)
	}
	if s.nextClient == nil {
		return nil // smo na repu
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// mark as internal replication call
	ctx = metadata.AppendToOutgoingContext(ctx, "x-internal-replication", "true")
	return fn(ctx)
}

// ensureNextClient dobi succesorja ce ga se nimamo
func (s *messageBoardServer) ensureNextClient() error {
	// ce ze mamo ne rab nc vracat
	if s.nextClient != nil {
		return nil
	}

	s.nextDialMu.Lock()
	defer s.nextDialMu.Unlock()
	//zakaj se enkrat daa ni kaka druga gorutina slucajn sla pisat kej in mi svinjat pac se en check ne skodi za vsak slucaj pac nrdimo
	if s.nextClient != nil {
		return nil
	}

	// Ce se ne vemo succesorja kljici control plane in ga dub ce obstaja
	if s.nextAddr == "" && s.controlPlaneAddr != "" {
		conn, err := grpc.NewClient(s.controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			cp := pb.NewControlPlaneClient(conn)
			state, err := cp.GetClusterState(context.Background(), &emptypb.Empty{})
			conn.Close()
			if err == nil {
				for _, cn := range state.GetChain() {
					if cn.GetInfo().GetAddress() == s.nodeInfo.GetAddress() {
						s.nextAddr = cn.GetNext()
						break
					}
				}
			}
		}
	}

	if s.nextAddr == "" {
		return nil // smo se vedn na repu
	}

	conn, err := grpc.NewClient(s.nextAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	s.nextConn = conn
	s.nextClient = pb.NewMessageBoardClient(conn)
	return nil
}

// za replikacijo user creata ka vedn gledas to na hedu da je res head in za pole notranjo replikacijo je to hitr fix ker pac me je metal vn pole za popravt
func (s *messageBoardServer) applyCreateUser(req *pb.CreateUserRequest) *pb.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextUserID++
	user := &pb.User{
		Id:   s.nextUserID,
		Name: req.GetName(),
	}
	s.users[user.Id] = user
	return user
}

// rpc CreateUser(CreateUserRequest) returns (User);
func (s *messageBoardServer) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.User, error) {
	// Check if external client call (not internal replication)
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	log.Printf("CreateUser on %s for user %s", s.nodeInfo.GetAddress(), req.GetName())
	// Add to this node/server
	user := s.applyCreateUser(req)
	log.Printf("Applied on %s, user ID=%d", s.nodeInfo.GetAddress(), user.Id)

	// poslji naslednjiku
	if err := s.replicate(func(ctx2 context.Context) error {
		log.Printf("Forwarding CreateUser to successor")
		_, err := s.nextClient.CreateUser(ctx2, req)
		return err
	}); err != nil {
		log.Printf("Replication failed: %v", err)
		return nil, err
	}
	return user, nil
}

// ista fora ku za userja i guess bom sam to delu ka idk ku drgace ka rabim spremljat ce pisem na pravo mesto ma pole bi rabu dat v message ce je blo ze cekirano na headu ka ku naj vem al je ta req prsu od clienta al serverja pac ja bullshit ma ajde
func (s *messageBoardServer) applyCreateTopic(req *pb.CreateTopicRequest) *pb.Topic {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextTopicID++
	topic := &pb.Topic{
		Id:   s.nextTopicID,
		Name: req.GetName(),
	}
	s.topics[topic.Id] = topic
	return topic
}

// rpc CreateTopic(CreateTopicRequest) returns (Topic);
func (s *messageBoardServer) CreateTopic(ctx context.Context, req *pb.CreateTopicRequest) (*pb.Topic, error) {
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	topic := s.applyCreateTopic(req)

	if err := s.replicate(func(ctx2 context.Context) error {
		_, err := s.nextClient.CreateTopic(ctx2, req)
		return err
	}); err != nil {
		return nil, err
	}
	return topic, nil
}

// applyPostMessage applies the post message operation
func (s *messageBoardServer) applyPostMessage(req *pb.PostMessageRequest) (*pb.Message, error) {
	s.mu.RLock()
	if _, ok := s.users[req.UserId]; !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("user does not exist")
	}
	if _, ok := s.topics[req.TopicId]; !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("topic does not exist")
	}
	s.mu.RUnlock()

	s.mu.Lock()
	s.nextMsgID++
	message := &pb.Message{
		Id:        s.nextMsgID,
		TopicId:   req.TopicId,
		UserId:    req.UserId,
		Text:      req.Text,
		CreatedAt: timestamppb.Now(),
		Likes:     0,
		Liked:     []int64{},
		Ver:       0,
		Dirty:     true,
	}
	s.messages[req.TopicId] = append(s.messages[req.TopicId], message)

	event := &pb.MessageEvent{
		SequenceNumber: message.Id,
		Op:             pb.OpType_OP_POST,
		Message:        message,
		EventAt:        timestamppb.Now(),
	}
	subscribers := s.subscribers[req.TopicId]
	s.mu.Unlock()

	for _, sub := range subscribers {
		sub.Send(event)
	}
	return message, nil
}

// rpc PostMessage(PostMessageRequest) returns (Message);
func (s *messageBoardServer) PostMessage(ctx context.Context, req *pb.PostMessageRequest) (*pb.Message, error) {
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	message, err := s.applyPostMessage(req)
	if err != nil {
		return nil, err
	}

	if err := s.replicate(func(rctx context.Context) error {
		_, err := s.nextClient.PostMessage(rctx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return message, nil
}

// applyUpdateMessage applies the update message operation
func (s *messageBoardServer) applyUpdateMessage(req *pb.UpdateMessageRequest) (*pb.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages, ok := s.messages[req.TopicId]
	if !ok {
		return nil, fmt.Errorf("topic does not exist")
	}

	for i, msg := range messages {
		if msg.Id == req.MessageId {
			if msg.UserId != req.UserId {
				return nil, fmt.Errorf("user is not the owner of this message")
			}
			message := &pb.Message{
				Id:        msg.Id,
				TopicId:   msg.TopicId,
				UserId:    msg.UserId,
				Text:      req.Text,
				CreatedAt: timestamppb.Now(),
				Likes:     msg.Likes,
				Liked:     msg.Liked,
				Ver:       msg.Ver + 1,
			}
			s.messages[req.TopicId][i] = message
			return message, nil
		}
	}
	return nil, fmt.Errorf("message with this id does not exist")
}

// rpc UpdateMessage(UpdateMessageRequest) returns (Message);
func (s *messageBoardServer) UpdateMessage(ctx context.Context, req *pb.UpdateMessageRequest) (*pb.Message, error) {
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	message, err := s.applyUpdateMessage(req)
	if err != nil {
		return nil, err
	}

	if err := s.replicate(func(rctx context.Context) error {
		_, err := s.nextClient.UpdateMessage(rctx, req)
		return err
	}); err != nil {
		return nil, err
	}

	return message, nil
}

//  rpc DeleteMessage(DeleteMessageRequest) returns (google.protobuf.Empty);

// applyDeleteMessage applies the delete message operation
func (s *messageBoardServer) applyDeleteMessage(req *pb.DeleteMessageRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages, ok := s.messages[req.TopicId]
	if !ok {
		return fmt.Errorf("topic does not exist")
	}

	for i, msg := range messages {
		if msg.Id == req.MessageId {
			if msg.UserId != req.UserId {
				return fmt.Errorf("user is not the owner of this message")
			}
			s.messages[req.TopicId] = append(messages[:i], messages[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("message with this id does not exist")
}

func (s *messageBoardServer) DeleteMessage(ctx context.Context, req *pb.DeleteMessageRequest) (*emptypb.Empty, error) {
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	if err := s.applyDeleteMessage(req); err != nil {
		return nil, err
	}

	if err := s.replicate(func(rctx context.Context) error {
		_, err := s.nextClient.DeleteMessage(rctx, req)
		return err
	}); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

// applyLikeMessage applies the like message operation
func (s *messageBoardServer) applyLikeMessage(req *pb.LikeMessageRequest) (*pb.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages, ok := s.messages[req.TopicId]
	if !ok {
		return nil, fmt.Errorf("topic does not exist")
	}
	for _, msg := range messages {
		if msg.Id == req.MessageId {
			if msg.UserId == req.UserId {
				return nil, fmt.Errorf("you can't like your own message")
			}
			for _, userid := range msg.Liked {
				if int64(userid) == req.UserId {
					return nil, fmt.Errorf("you already liked this message")
				}
			}
			msg.Liked = append(msg.Liked, req.UserId)
			msg.Likes++
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message with this id not found")
}

// rpc LikeMessage(LikeMessageRequest) returns (Message);
func (s *messageBoardServer) LikeMessage(ctx context.Context, req *pb.LikeMessageRequest) (*pb.Message, error) {
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}
	message, err := s.applyLikeMessage(req)
	if err != nil {
		return nil, err
	}

	if err := s.replicate(func(rctx context.Context) error {
		_, err := s.nextClient.LikeMessage(rctx, req)
		return err
	}); err != nil {
		return nil, err
	}

	return message, nil
}

// ///////////////////////////
// rpc GetSubscriptionNode(SubscriptionNodeRequest) returns (SubscriptionNodeResponse);
func (s *messageBoardServer) GetSubscriptionNode(ctx context.Context, req *pb.SubscriptionNodeRequest) (*pb.SubscriptionNodeResponse, error) {
	return &pb.SubscriptionNodeResponse{
		SubscribeToken: "OK",
		Node:           s.nodeInfo,
	}, nil
}

// rpc ListTopics(google.protobuf.Empty) returns (ListTopicsResponse);
func (s *messageBoardServer) ListTopics(ctx context.Context, req *emptypb.Empty) (*pb.ListTopicsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &pb.ListTopicsResponse{}
	for _, topic := range s.topics {
		resp.Topics = append(resp.Topics, topic)
	}
	return resp, nil
}

// rpc GetMessages(GetMessagesRequest) returns (GetMessagesResponse); vrne vse message v topicu
func (s *messageBoardServer) GetMessages(ctx context.Context, req *pb.GetMessagesRequest) (*pb.GetMessagesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &pb.GetMessagesResponse{}
	//isto ku ce gres skozi vse nrdi basicly spread operator uporablji(zapomni si da je ta rec tud v go)
	resp.Messages = append(resp.Messages, s.messages[req.TopicId]...)
	return resp, nil
}

// refresha vsakih 5 secund in prasa controlPlane ej kaka je situacija pr tebi kaj se je kdo nov prjavu....
func (s *messageBoardServer) startTopologyRefresh() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			conn, err := grpc.NewClient(s.controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				continue
			}
			cp := pb.NewControlPlaneClient(conn)
			//dubi state
			state, err := cp.GetClusterState(context.Background(), &emptypb.Empty{})
			//zpri povezavo!!!!!!!
			conn.Close()
			if err != nil {
				continue
			}

			// update head tail statuse
			s.mu.Lock()
			s.isHead = state.Head.Address == s.nodeInfo.GetAddress()
			s.isTail = state.Tail.Address == s.nodeInfo.GetAddress()

			for _, cn := range state.GetChain() {
				if cn.GetInfo().GetAddress() == s.nodeInfo.GetAddress() {
					newNextAddr := cn.GetNext()
					if newNextAddr != s.nextAddr {
						s.nextAddr = newNextAddr
						if s.nextConn != nil {
							s.nextConn.Close()
						}
						s.nextClient = nil //force reconect na naslednjem pisanju.... zaenkrat pole za sprement mogoc
					}
				}
			}
			s.mu.Unlock()
		}
	}()
}

// rpc SubscribeTopic(SubscribeTopicRequest) returns (stream MessageEvent);
func (s *messageBoardServer) SubscribeTopic(req *pb.SubscribeTopicRequest, stream pb.MessageBoard_SubscribeTopicServer) error {
	// register subscriber for each topic
	s.mu.Lock()
	for _, topicID := range req.TopicId {
		if _, ok := s.topics[topicID]; !ok {
			s.mu.Unlock()
			return fmt.Errorf("the topic with this ID does not exist")
		}
		s.subscribers[topicID] = append(s.subscribers[topicID], stream)
	}
	s.mu.Unlock()

	log.Printf("Client subscribed to topics: %v", req.TopicId)

	// držimo stream odprt dokler se uporabnik ne odklopi
	<-stream.Context().Done()

	// Clean up: remove subscriber when client disconnects
	s.mu.Lock()
	for _, topicID := range req.TopicId {
		subs := s.subscribers[topicID]
		for i, sub := range subs {
			if sub == stream {
				s.subscribers[topicID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()

	log.Printf("Client unsubscribed from topics: %v", req.TopicId)
	return nil
}

// rpc GetSyncStream(GetSyncStreamRequest) returns (stream SyncEvent);
func (s *messageBoardServer) GetSyncStream(req *pb.GetSyncStreamRequest, stream pb.MessageBoard_GetSyncStreamServer) error {
	s.mu.RLock()
	defer s.mu.RUnlock() // če hočemo unblocking mora bit lock unlock okoli vsakega posebej v loopu
	// Send users
	for _, user := range s.users {
		if err := stream.Send(&pb.SyncEvent{Data: &pb.SyncEvent_User{User: user}}); err != nil {
			return err
		}
	}
	// Send topics
	for _, topic := range s.topics {
		if err := stream.Send(&pb.SyncEvent{Data: &pb.SyncEvent_Topic{Topic: topic}}); err != nil {
			return err
		}
	}
	// Send messages
	for _, t := range s.topics {
		for _, msg := range s.messages[t.GetId()] {
			if err := stream.Send(&pb.SyncEvent{Data: &pb.SyncEvent_Message{Message: msg}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	addrFlag := flag.String("addr", "localhost:50051", "address this node listens on")
	controlPlaneFlag := flag.String("control-plane", "localhost:6000", "control plane address")
	flag.Parse()

	myAddr := *addrFlag
	controlPlaneAddr := *controlPlaneFlag

	node := &pb.NodeInfo{
		Address: myAddr,
	}

	// connect to control plane
	conn, err := grpc.NewClient(controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	cp := pb.NewControlPlaneClient(conn)
	// connect to contrl plane to get tail addres, connect to tail and start copying data
	// if not nil
	// must copy nextUserID, nextTopicID, nextMsgID, users, topics, messages
	users := make(map[int64]*pb.User)
	topics := make(map[int64]*pb.Topic)
	messages := make(map[int64][]*pb.Message)
	var nextUserID int64 = 0
	var nextTopicID int64 = 0
	var nextMsgID int64 = 0
	var skip bool = false
	state, err := cp.GetClusterState(context.Background(), &emptypb.Empty{})
	if err != nil {
		st, ok := status.FromError(err)
		if ok {
			if st.Message() == "no nodes registered" {
				skip = true
			} else {
				log.Fatal("tail connection failed:", err)
			}
		}
	}
	if !skip {
		tailAddr := state.GetTail().GetAddress()
		tailConn, err := grpc.NewClient(tailAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatal("tail connection failed:", err)
		}
		tailClient := pb.NewMessageBoardClient(tailConn)
		req := &pb.GetSyncStreamRequest{
			UserId:    0,
			TopicId:   0,
			MessageId: 0,
		}
		stream, err := tailClient.GetSyncStream(context.Background(), req) // na tem streamu bodo podatki, ki jih morš syncat
		if err != nil {
			log.Fatal("tail connection failed:", err)
		}
		for {
			//Recv() sprejme naslednji response from server
			event, err := stream.Recv()
			//EOF se poslje na koncu ko se streem terminata
			if err == io.EOF {
				fmt.Println("Synchronization complete")
				break
			}
			if err != nil {
				fmt.Println("Stream error:", err)
				return
			}
			switch event.Data.(type) {
			case *pb.SyncEvent_User:
				users[event.GetUser().GetId()] = event.GetUser()
			case *pb.SyncEvent_Topic:
				topics[event.GetTopic().GetId()] = event.GetTopic()
			case *pb.SyncEvent_Message:
				fmt.Println(event.GetMessage())
				messages[event.GetMessage().TopicId] = append(messages[event.GetMessage().TopicId], event.GetMessage())
			}
		}
		// posodobi next user,topic,msg id
		for _, u := range users {
			nextUserID = max(u.Id, nextUserID)
		}
		for _, t := range topics {
			nextTopicID = max(t.Id, nextTopicID)
			for _, m := range messages[t.Id] {
				nextMsgID = max(m.Id, nextMsgID)
			}
		}

	}
	// register node
	rnr, err := cp.RegisterNode(context.Background(), node)
	if err != nil {
		log.Fatal("register failed:", err)
	} else {
		node.NodeId = rnr.NodeId
	}

	// zaženi heartbeat v ozadju
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_, err := cp.Heartbeat(context.Background(), node)
				if err != nil {
					log.Printf("heartbeat failed: %v", err)
				}
			default:
				continue
			}
		}
	}()

	// cluster state iz control plaina
	state, err = cp.GetClusterState(context.Background(), &emptypb.Empty{})
	if err != nil {
		log.Fatal(err)
	}

	isHead := state.Head.Address == myAddr
	isTail := state.Tail.Address == myAddr

	nextAddr := ""
	for _, cn := range state.GetChain() {
		if cn.GetInfo().GetAddress() == myAddr {
			nextAddr = cn.GetNext()
			break
		}
	}

	srv := &messageBoardServer{
		nodeInfo:         node,
		isHead:           isHead,
		isTail:           isTail,
		controlPlaneAddr: controlPlaneAddr,
		nextUserID:       nextUserID,
		nextTopicID:      nextTopicID,
		nextMsgID:        nextMsgID,
		users:            users,
		topics:           topics,
		messages:         messages,
		subscribers:      make(map[int64][]pb.MessageBoard_SubscribeTopicServer),
		nextAddr:         nextAddr,
	}
	srv.startTopologyRefresh()
	if nextAddr != "" {
		nextConn, err := grpc.NewClient(nextAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("failed to connect to next %s: %v", nextAddr, err)
		}
		srv.nextClient = pb.NewMessageBoardClient(nextConn)
		srv.nextConn = nextConn
	}

	grpcServer := grpc.NewServer()
	pb.RegisterMessageBoardServer(grpcServer, srv)

	lis, err := net.Listen("tcp", myAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", myAddr, err)
	}
	log.Println("Node running at", myAddr, "head:", isHead, "tail:", isTail)
	/*
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for range ticker.C {
				srv.printAll()
			}
		}()*/
	grpcServer.Serve(lis)
}

//writes go to head, reads go to tail
