package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	pb "razpravljalnica/proto"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt" //isto ku na javascript obstaja bcrypt tko da bom realn zasifriru gesla ka poznam ze library
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
	//isto ku za next tud za previous
	prevClient pb.MessageBoardClient // connection to previous node (for acks) --za dirty bit
	prevConn   *grpc.ClientConn      // connection to previous
	prevAddr   string                // address of previous node

	cpAddrs []string

	// Dirty bit za replikacijo
	opSeqCounter int64                     // za vedet ali je operacija ze bla complited ali ne mal lazje za tracat torej pac posljes idk ack z stevilko 1 in pole nazaj tudi ack s stevilko 1 da ves kiri biti niso vec dirty
	dirtyOps     map[int64]*dirtyOperation // to pa shrani operacijo ki je bla nrjena oziroma kaj je treba replicirat
	dirtyMu      sync.RWMutex              // za zaklepanje (RWMutex for read/write locks)
	appliedOps   map[int64]bool            // track already applied operations to prevent duplicates during replication
	appliedMu    sync.RWMutex
}

// dirtyOperation tracks an operation that hasn't been fully replicated
type dirtyOperation struct {
	OpType string
	Data   interface{}
}

func getCPAddr(cpAddrs []string) string {
	log.Printf("Looking for control plane leader among: %v", cpAddrs)
	for { // ponavlja dokler ne dobi leaderja
		for _, addr := range cpAddrs {
			log.Printf("Trying control plane at %s...", addr)
			cpConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil { // poskusimo drugi naslov
				log.Printf("Failed to create client for %s: %v", addr, err)
				continue
			}
			cp := pb.NewControlPlaneClient(cpConn)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			res, err := cp.GetLeaderAddr(ctx, &emptypb.Empty{})
			cancel()
			cpConn.Close()                                        // close immediately, not with defer
			if err == nil && res != nil && res.LeaderAddr != "" { // found leader
				log.Printf("Found control plane leader at %s", res.LeaderAddr)
				return res.LeaderAddr
			}
			log.Printf("No valid leader response from %s (err=%v, res=%v)", addr, err, res)
		}
		log.Printf("No control plane leader found, retrying in 1 second...")
		time.Sleep(1 * time.Second)
	}
}

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

// ali je iy serverja ali clienta check zato ka rabim vedet da se je na head v prvo poslalo in pole po tem pac dodas en podpis da ves da je ok - chatko pomagu
func isInternalCall(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	vals := md.Get("x-internal-replication")
	return len(vals) > 0 && vals[0] == "true"
}

// dobi sequence number iz opperationa in ali je dirty ali ne isto ku uno ka prpopam na message ali je prvic blo napisano ali ne
func getOperationSeq(ctx context.Context) int64 {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0
	}
	vals := md.Get("x-operation-seq")
	if len(vals) == 0 {
		return 0
	}
	var seq int64
	fmt.Sscanf(vals[0], "%d", &seq)
	return seq
}

// replicate gre po chainu in replicata podatke tail neha replicatat ka nima vec naslednjika to je sam helper dejsnski replicate se izvaja zdravn zapisov
func (s *messageBoardServer) replicate(opSeq int64, fn func(ctx context.Context) error) error {
	// dubi naslednjika ce ne ves za njegov obstoj
	if err := s.ensureNextClient(); err != nil {
		return fmt.Errorf("replication failed (next unavailable): %w", err)
	}
	if s.nextClient == nil {
		return nil // smo na repu
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// mark as internal replication call and pass operation sequence
	//ta internal je da ves da je ze blo napisano
	ctx = metadata.AppendToOutgoingContext(ctx, "x-internal-replication", "true")
	if opSeq > 0 {
		//to je za dirty sam sequence number
		ctx = metadata.AppendToOutgoingContext(ctx, "x-operation-seq", fmt.Sprintf("%d", opSeq))
	}
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

// da se mi ne podvaja ce slucajn je ze bil opperation applied
func (s *messageBoardServer) isAlreadyApplied(opSeq int64) bool {
	if opSeq == 0 {
		return false
	}
	s.appliedMu.RLock()
	defer s.appliedMu.RUnlock()
	return s.appliedOps[opSeq]
}

// da marka opperation ass applied da ne pisem povsod ka se povsod ponavlja
func (s *messageBoardServer) markApplied(opSeq int64) {
	if opSeq > 0 {
		s.appliedMu.Lock()
		s.appliedOps[opSeq] = true
		s.appliedMu.Unlock()
	}
}

// dodaj username z passwordom in imenom (heshiraj password)
func (s *messageBoardServer) applyRegisterUser(req *pb.RegisterRequest) (*pb.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if user already exists
	for _, u := range s.users {
		if u.Name == req.GetUsername() {
			return nil, fmt.Errorf("username already exists")
		}
	}

	// Hash the password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.GetPassword()), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	// Create new user
	s.nextUserID++
	user := &pb.User{
		Id:       s.nextUserID,
		Name:     req.GetUsername(),
		Password: string(hashedPassword),
	}
	s.users[user.Id] = user
	return user, nil
}

// registriraj ga
func (s *messageBoardServer) RegisterUser(ctx context.Context, req *pb.RegisterRequest) (*pb.User, error) {
	// Check if external client call (not internal replication)
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
	}

	// chekeraj ce je external client in ne internal replication
	var opSeq int64
	if !isInternalCall(ctx) {
		opSeq = s.markDirty("RegisterUser", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	var user *pb.User
	var err error

	if !s.isAlreadyApplied(opSeq) {
		user, err = s.applyRegisterUser(req)
		if err != nil {
			return nil, err
		}
		log.Printf("Applied RegisterUser on %s: user_id=%d, username=%s (opSeq=%d)",
			s.nodeInfo.GetAddress(), user.Id, user.Name, opSeq)
		s.markApplied(opSeq)
	} else {
		// user je ze registriran
		s.mu.RLock()
		for _, u := range s.users {
			if u.Name == req.GetUsername() {
				user = u
				break
			}
		}
		s.mu.RUnlock()
		if user == nil {
			return nil, fmt.Errorf("user registration failed")
		}
	}

	// replikacija
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(ctx2 context.Context) error {
			_, err := s.nextClient.RegisterUser(ctx2, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// ack iz taila
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "RegisterUser")
	}
	// ce je head in tail sam zbrisi dirty da ne konstantn posilja in isce
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
	}

	return user, nil
}

// login
func (s *messageBoardServer) LoginUser(ctx context.Context, req *pb.LoginRequest) (*pb.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find user by username
	var user *pb.User
	for _, u := range s.users {
		if u.Name == req.GetUsername() {
			user = u
			break
		}
	}

	if user == nil {
		return nil, fmt.Errorf("invalid username or password")
	}

	// bycript preveri
	err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.GetPassword()))
	if err != nil {
		return nil, fmt.Errorf("invalid username or password")
	}

	// vrni userja brez passworda
	return &pb.User{
		Id:   user.Id,
		Name: user.Name,
	}, nil
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
	var opSeq int64

	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
		opSeq = s.markDirty("CreateTopic", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	if !s.isAlreadyApplied(opSeq) {
		// Check if topic with this name already exists
		s.mu.RLock()
		var existingTopic *pb.Topic
		for _, t := range s.topics {
			if t.Name == req.GetName() {
				existingTopic = t
				break
			}
		}
		s.mu.RUnlock()

		if existingTopic == nil {
			topic := s.applyCreateTopic(req)
			log.Printf("Applied CreateTopic on %s: topic_id=%d, name=%s (opSeq=%d, isInternal=%v)",
				s.nodeInfo.GetAddress(), topic.Id, topic.Name, opSeq, isInternalCall(ctx))
			s.markApplied(opSeq)
		} else {
			log.Printf("CreateTopic: topic %s already exists locally, will return it but NOT marking opSeq=%d as applied", req.GetName(), opSeq)
		}
	} else {
		log.Printf("CreateTopic opSeq=%d already applied on %s, skipping", opSeq, s.nodeInfo.GetAddress())
	}

	// Find topic to return
	var topic *pb.Topic
	s.mu.RLock()
	for _, t := range s.topics {
		if t.Name == req.GetName() {
			topic = t
			break
		}
	}
	s.mu.RUnlock()

	// Replicate only if not tail
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(ctx2 context.Context) error {
			_, err := s.nextClient.CreateTopic(ctx2, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// Send ACK if tail
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "CreateTopic")
	}

	// If we're both head AND tail (single node), clear dirty immediately
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
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
	var opSeq int64
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
		opSeq = s.markDirty("PostMessage", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	var message *pb.Message
	var err error

	if !s.isAlreadyApplied(opSeq) {
		message, err = s.applyPostMessage(req)
		if err != nil {
			//ce error ni nil brisi vn iz replikacije da ne v neskoncno repliciramo
			if opSeq > 0 {
				s.dirtyMu.Lock()
				delete(s.dirtyOps, opSeq)
				s.dirtyMu.Unlock()
			}
			log.Printf("Deleted dirty on (opSeq=%d, isInternal=%v) because of err: %v", opSeq, isInternalCall(ctx), err)
			return nil, err
		}
		log.Printf("Applied PostMessage on %s: msg_id=%d, text=%s (opSeq=%d, isInternal=%v)",
			s.nodeInfo.GetAddress(), message.Id, message.Text, opSeq, isInternalCall(ctx))
		s.markApplied(opSeq)
	} else {
		log.Printf("PostMessage opSeq=%d already applied on %s, skipping", opSeq, s.nodeInfo.GetAddress())
		// Find existing message
		s.mu.RLock()
		for _, msg := range s.messages[req.TopicId] {
			if msg.Text == req.Text && msg.UserId == req.UserId {
				message = msg
				break
			}
		}
		s.mu.RUnlock()
	}

	// Replicate only if not tail
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(rctx context.Context) error {
			_, err := s.nextClient.PostMessage(rctx, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// Send ACK if tail
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "PostMessage")
	}

	// If we're both head AND tail (single node), clear dirty immediately
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
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
	var opSeq int64
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
		opSeq = s.markDirty("UpdateMessage", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	var message *pb.Message
	var err error

	if !s.isAlreadyApplied(opSeq) {
		message, err = s.applyUpdateMessage(req)
		if err != nil {
			//ce error ni nil brisi vn iz replikacije da ne v neskoncno repliciramo
			if opSeq > 0 {
				s.dirtyMu.Lock()
				delete(s.dirtyOps, opSeq)
				s.dirtyMu.Unlock()
			}
			log.Printf("Deleted dirty on (opSeq=%d, isInternal=%v) because of err: %v", opSeq, isInternalCall(ctx), err)
			return nil, err
		}
		log.Printf("Applied UpdateMessage on %s: msg_id=%d, text=%s (opSeq=%d, isInternal=%v)",
			s.nodeInfo.GetAddress(), message.Id, message.Text, opSeq, isInternalCall(ctx))
		s.markApplied(opSeq)
	} else {
		log.Printf("UpdateMessage opSeq=%d already applied on %s, skipping", opSeq, s.nodeInfo.GetAddress())
		s.mu.RLock()
		for _, msg := range s.messages[req.TopicId] {
			if msg.Id == req.MessageId {
				message = msg
				break
			}
		}
		s.mu.RUnlock()
	}

	// Replicate only if not tail
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(rctx context.Context) error {
			_, err := s.nextClient.UpdateMessage(rctx, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// Send ACK if tail
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "UpdateMessage")
	}

	// If we're both head AND tail (single node), clear dirty immediately
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
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
	var opSeq int64
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
		opSeq = s.markDirty("DeleteMessage", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	if !s.isAlreadyApplied(opSeq) {
		if err := s.applyDeleteMessage(req); err != nil {
			//ce error ni nil brisi vn iz replikacije da ne v neskoncno repliciramo
			if opSeq > 0 {
				s.dirtyMu.Lock()
				delete(s.dirtyOps, opSeq)
				s.dirtyMu.Unlock()
			}
			log.Printf("Deleted dirty on (opSeq=%d, isInternal=%v) because of err: %v", opSeq, isInternalCall(ctx), err)
			return nil, err
		}
		log.Printf("Applied DeleteMessage on %s: msg_id=%d (opSeq=%d, isInternal=%v)",
			s.nodeInfo.GetAddress(), req.MessageId, opSeq, isInternalCall(ctx))
		s.markApplied(opSeq)
	} else {
		log.Printf("DeleteMessage opSeq=%d already applied on %s, skipping", opSeq, s.nodeInfo.GetAddress())
	}

	// Replicate only if not tail
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(rctx context.Context) error {
			_, err := s.nextClient.DeleteMessage(rctx, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// Send ACK if tail
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "DeleteMessage")
	}

	// If we're both head AND tail (single node), clear dirty immediately
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
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
	var opSeq int64
	// Check if external client call
	if !isInternalCall(ctx) {
		s.mu.RLock()
		isHead := s.isHead
		s.mu.RUnlock()
		if !isHead {
			return nil, fmt.Errorf("write operation must be sent to head node")
		}
		opSeq = s.markDirty("LikeMessage", req)
	} else {
		opSeq = getOperationSeq(ctx)
	}

	var message *pb.Message
	var err error

	if !s.isAlreadyApplied(opSeq) {
		message, err = s.applyLikeMessage(req)
		if err != nil {
			//ce error ni nil brisi vn iz replikacije da ne v neskoncno repliciramo
			if opSeq > 0 {
				s.dirtyMu.Lock()
				delete(s.dirtyOps, opSeq)
				s.dirtyMu.Unlock()
			}
			log.Printf("Deleted dirty on (opSeq=%d, isInternal=%v) because of err: %v", opSeq, isInternalCall(ctx), err)
			return nil, err
		}
		log.Printf("Applied LikeMessage on %s: msg_id=%d (opSeq=%d, isInternal=%v)",
			s.nodeInfo.GetAddress(), req.MessageId, opSeq, isInternalCall(ctx))
		s.markApplied(opSeq)
	} else {
		log.Printf("LikeMessage opSeq=%d already applied on %s, skipping", opSeq, s.nodeInfo.GetAddress())
		s.mu.RLock()
		for _, msg := range s.messages[req.TopicId] {
			if msg.Id == req.MessageId {
				message = msg
				break
			}
		}
		s.mu.RUnlock()
	}

	// Replicate only if not tail
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	if !isTail {
		if err := s.replicate(opSeq, func(rctx context.Context) error {
			_, err := s.nextClient.LikeMessage(rctx, req)
			return err
		}); err != nil {
			log.Printf("Replication failed: %v (will retry when topology stabilizes)", err)
		}
	}

	// Send ACK if tail
	if isTail && isInternalCall(ctx) && opSeq > 0 {
		go s.sendAckToPrev(opSeq, "LikeMessage")
	}

	// If we're both head AND tail (single node), clear dirty immediately
	if !isInternalCall(ctx) && isTail && opSeq > 0 {
		s.dirtyMu.Lock()
		delete(s.dirtyOps, opSeq)
		s.dirtyMu.Unlock()
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
			// ne vem vec kaj ce je pac dirty operacija probavi pac prepisat samo meci dokler niso vsi ackji potrjeni mal tko ma pomojem je ok
			s.mu.RLock()
			isHead := s.isHead
			s.mu.RUnlock()

			if isHead {
				s.dirtyMu.RLock()
				hasDirty := len(s.dirtyOps) > 0
				s.dirtyMu.RUnlock()

				if hasDirty {
					s.mu.RLock()
					isTail := s.isTail
					s.mu.RUnlock()

					// If head=tail (single-node cluster), clear dirty immediately
					if isTail {
						log.Printf("Head is also tail (single-node), clearing dirty operations immediately")
						s.dirtyMu.Lock()
						s.dirtyOps = make(map[int64]*dirtyOperation)
						s.dirtyMu.Unlock()
					} else {
						log.Printf("Head has dirty operations, attempting replication")
						s.reReplicateDirtyOps()
					}
				}
			}

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
			//za topology refresh mal lazje za primerjat pole
			oldIsHead := s.isHead
			oldIsTail := s.isTail
			oldNextAddr := s.nextAddr
			oldPrevAddr := s.prevAddr

			s.isHead = state.Head.Address == s.nodeInfo.GetAddress()
			s.isTail = state.Tail.Address == s.nodeInfo.GetAddress()

			for _, cn := range state.GetChain() {
				if cn.GetInfo().GetAddress() == s.nodeInfo.GetAddress() {
					newNextAddr := cn.GetNext()
					newPrevAddr := cn.GetPrev()

					// Handle next address change
					if newNextAddr != s.nextAddr {
						log.Printf("Detected topology change: next was %s, now %s", s.nextAddr, newNextAddr)
						s.nextAddr = newNextAddr
						if s.nextConn != nil {
							s.nextConn.Close()
							s.nextConn = nil
						}
						s.nextClient = nil //force reconect na naslednjem pisanju.... zaenkrat pole za sprement mogoc

						// provi takoj vzpostavit conn z novim node-om
						if s.nextAddr != "" {
							conn, err := grpc.NewClient(s.nextAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
							if err == nil {
								s.nextConn = conn
								s.nextClient = pb.NewMessageBoardClient(conn)
								log.Printf("Established connection to new next node at %s", s.nextAddr)
							} else {
								log.Printf("Failed to establish connection to new next node at %s: %v", s.nextAddr, err)
							}
						}
					}

					// za prejsnji addr ce se je spremenilo ist ku prej ka sm mogu za next
					if newPrevAddr != s.prevAddr {
						log.Printf("Detected topology change: prev was %s, now %s", s.prevAddr, newPrevAddr)
						s.prevAddr = newPrevAddr
						if s.prevConn != nil {
							s.prevConn.Close()
						}
						s.prevClient = nil
						// Nov connection do prejsnjega ce ga dobimo
						if s.prevAddr != "" {
							prevConn, err := grpc.NewClient(s.prevAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
							if err == nil {
								s.prevConn = prevConn
								s.prevClient = pb.NewMessageBoardClient(prevConn)
								log.Printf("Established connection to previous node at %s", s.prevAddr)
							}
						}
					}
				}
			}
			s.mu.Unlock()

			// Izpisi ce se je kej spremenilo za debuging
			if oldIsHead != s.isHead || oldIsTail != s.isTail {
				log.Printf("Role changed: isHead=%v, isTail=%v", s.isHead, s.isTail)
			}
			if oldNextAddr != s.nextAddr {
				log.Printf("Next address changed: %s -> %s", oldNextAddr, s.nextAddr)
			}
			if oldPrevAddr != s.prevAddr {
				log.Printf("Prev address changed: %s -> %s", oldPrevAddr, s.prevAddr)
			}
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

// to je vse za replikacijo pole nazaj od taila
func (s *messageBoardServer) AckOperation(ctx context.Context, req *pb.AckOperationRequest) (*emptypb.Empty, error) {
	log.Printf("Received ACK for operation %d (%s) on node %s", req.OperationSeq, req.OperationType, s.nodeInfo.GetAddress())

	// ce smo head pobrisi dirty bit edina pomembna rec
	s.mu.RLock()
	isHead := s.isHead
	s.mu.RUnlock()

	if isHead {
		s.dirtyMu.Lock()
		if _, exists := s.dirtyOps[req.OperationSeq]; exists {
			log.Printf("Head received ACK for operation %d (%s), clearing dirty bit", req.OperationSeq, req.OperationType)
			delete(s.dirtyOps, req.OperationSeq)
		} else {
			log.Printf("Head received ACK for operation %d but it was not in dirty set (the fuck happened here)", req.OperationSeq)
		}
		s.dirtyMu.Unlock()
	} else {
		// if not head get that opperation back to prev node
		log.Printf("Forwarding ACK for operation %d to previous node", req.OperationSeq)
		go s.sendAckToPrev(req.OperationSeq, req.OperationType)
	}

	return &emptypb.Empty{}, nil
}

// to je da da operacijio pod dirty pac da jo oznaci ku dirty
func (s *messageBoardServer) markDirty(opType string, data interface{}) int64 {
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()

	s.opSeqCounter++
	seq := s.opSeqCounter
	s.dirtyOps[seq] = &dirtyOperation{
		OpType: opType,
		Data:   data,
	}
	log.Printf("Marked operation %d (%s) as dirty", seq, opType)
	return seq
}

// back po chanu da na prejsnji ack
func (s *messageBoardServer) sendAckToPrev(opSeq int64, opType string) error {
	if s.prevClient == nil || s.prevAddr == "" {
		return nil //smo head nimamo vec komu posiljat
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := s.prevClient.AckOperation(ctx, &pb.AckOperationRequest{
		OperationSeq:  opSeq,
		OperationType: opType,
	})
	if err != nil {
		log.Printf("Failed to send ACK to previous node for operation %d: %v", opSeq, err)
		return err
	}
	log.Printf("Sent ACK to previous node for operation %d (%s)", opSeq, opType)
	return nil
}

// vse reReplicatamo do novega succ ce je slo kej narobe
func (s *messageBoardServer) reReplicateDirtyOps() {
	s.mu.RLock()
	isTail := s.isTail
	s.mu.RUnlock()

	// If we're tail, we shouldn't be replicating - clear dirty ops and return
	if isTail {
		log.Printf("reReplicateDirtyOps called on tail node, clearing dirty operations")
		s.dirtyMu.Lock()
		s.dirtyOps = make(map[int64]*dirtyOperation)
		s.dirtyMu.Unlock()
		return
	}

	s.dirtyMu.Lock()
	dirtyOpsCopy := make(map[int64]*dirtyOperation)
	for seq, op := range s.dirtyOps {
		dirtyOpsCopy[seq] = op
	}
	s.dirtyMu.Unlock()

	if len(dirtyOpsCopy) == 0 {
		//neki ne dela sam zame ko gre dol ka se ne replicata me zanima ce misli da ni nobene dirty
		log.Printf("No dirty operations to replicate")
		return
	}

	log.Printf("replicating %d dirty operations due to topology change", len(dirtyOpsCopy))

	// cimprej poglej ce ze mamo nov client
	s.mu.RLock()
	hasClient := s.nextClient != nil
	nextAddr := s.nextAddr
	s.mu.RUnlock()

	if !hasClient {
		log.Printf("Next client is nil, attempting to establish connection to %s", nextAddr)
		if err := s.ensureNextClient(); err != nil {
			log.Printf("Failed to establish connection to new next node: %v", err)
			return
		}
	}

	s.mu.RLock()
	if s.nextClient == nil {
		s.mu.RUnlock()
		log.Printf("No next client available for replication (we might be tail now)") //skori bi pozabu mi ni delalo in nism vedu zakaj
		return
	}
	s.mu.RUnlock()

	// vsak dirty opp je treba replicatat
	for seq, op := range dirtyOpsCopy {
		log.Printf("replicating operation %d (%s)", seq, op.OpType)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ctx = metadata.AppendToOutgoingContext(ctx, "x-internal-replication", "true")
		ctx = metadata.AppendToOutgoingContext(ctx, "x-operation-seq", fmt.Sprintf("%d", seq))

		var err error
		//za vsak opperation je treba na naslednjem clientu poklicat neko operacijo pole bi moglo delat naprej tko ku ce bit
		switch op.OpType {
		//zakaj ni delalo i am asking myself zakaj se ne replicira fuck my life pac
		case "RegisterUser":
			req := op.Data.(*pb.RegisterRequest)
			_, err = s.nextClient.RegisterUser(ctx, req)
		case "CreateTopic":
			req := op.Data.(*pb.CreateTopicRequest)
			_, err = s.nextClient.CreateTopic(ctx, req)
		case "PostMessage":
			req := op.Data.(*pb.PostMessageRequest)
			_, err = s.nextClient.PostMessage(ctx, req)
		case "UpdateMessage":
			req := op.Data.(*pb.UpdateMessageRequest)
			_, err = s.nextClient.UpdateMessage(ctx, req)
		case "DeleteMessage":
			req := op.Data.(*pb.DeleteMessageRequest)
			_, err = s.nextClient.DeleteMessage(ctx, req)
		case "LikeMessage":
			req := op.Data.(*pb.LikeMessageRequest)
			_, err = s.nextClient.LikeMessage(ctx, req)
		default:
			log.Printf("Unknown operation type: %s", op.OpType)
		}
		cancel()

		if err != nil {
			log.Printf("Failed to replicate operation %d: %v", seq, err)
		} else {
			log.Printf("Successfully replicated operation %d", seq)
		}
	}
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

// addr == myAddr
func Run(addr string, cpAddrs []string) {
	log.Printf("Starting message board server...")
	log.Printf("Server will listen on: localhost:%s", addr)
	log.Printf("Control plane addresses: %v", cpAddrs)
	var controlPlaneAddr string = getCPAddr(cpAddrs)
	//zato da windows ni tecn in da lazje klices
	addr = "localhost:" + addr
	//controlPlaneAddr = "localhost:" + controlPlaneAddr
	//////////////////////////////////////
	node := &pb.NodeInfo{
		Address: addr,
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
		log.Fatal("Failed to register node:", err)
	}
	node.NodeId = rnr.NodeId

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
					// rediscover leader and replace shared connection/client
					newAddr := getCPAddr(cpAddrs)
					newConn, err := grpc.NewClient(newAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err != nil {
						log.Printf("control-plane reconnect failed: %v", err)
						continue
					}
					if conn != nil {
						conn.Close()
					}
					conn = newConn
					controlPlaneAddr = newAddr
					cp = pb.NewControlPlaneClient(conn)
					continue
				}
			default:
				continue
			}
		}
	}()

	// cluster state iz control plaina
	// Wait briefly until our registration is visible to the control plane
	{
		deadline := time.Now().Add(10 * time.Second)
		for {
			state, err = cp.GetClusterState(context.Background(), &emptypb.Empty{})
			if err == nil {
				break
			}
			st, ok := status.FromError(err)
			if ok && st.Message() == "no nodes registered" && time.Now().Before(deadline) {
				log.Printf("Cluster state not ready yet (no nodes registered), retrying...")
				time.Sleep(200 * time.Millisecond)
				continue
			}
			log.Fatal("Failed to get cluster state:", err)
		}
	}

	isHead := state.Head.Address == addr
	isTail := state.Tail.Address == addr

	nextAddr := ""
	prevAddr := ""
	for _, cn := range state.GetChain() {
		if cn.GetInfo().GetAddress() == addr {
			nextAddr = cn.GetNext()
			prevAddr = cn.GetPrev()
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
		prevAddr:         prevAddr,
		cpAddrs:          cpAddrs,
		dirtyOps:         make(map[int64]*dirtyOperation),
		opSeqCounter:     0,
		appliedOps:       make(map[int64]bool),
	}
	srv.startTopologyRefresh()

	// Connect to next node if available
	if nextAddr != "" {
		nextConn, err := grpc.NewClient(nextAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("failed to connect to next %s: %v", nextAddr, err)
		}
		srv.nextClient = pb.NewMessageBoardClient(nextConn)
		srv.nextConn = nextConn
	}

	// Connect to prev node if available (for sending acks backwards)
	if prevAddr != "" {
		prevConn, err := grpc.NewClient(prevAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Printf("Warning: failed to connect to prev %s: %v", prevAddr, err)
		} else {
			srv.prevClient = pb.NewMessageBoardClient(prevConn)
			srv.prevConn = prevConn
			log.Printf("Connected to previous node at %s for acknowledgments", prevAddr)
		}
	}

	grpcServer := grpc.NewServer()
	pb.RegisterMessageBoardServer(grpcServer, srv)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	log.Println("Node running at", addr, "head:", isHead, "tail:", isTail)
	grpcServer.Serve(lis)
}

//writes go to head, reads go to tail
