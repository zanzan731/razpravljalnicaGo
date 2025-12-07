package main

import (
	"context"
	"fmt"
	"log"
	"net"
	pb "razpravljalnica/proto"
	"sync"

	"google.golang.org/grpc"
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

	mu sync.RWMutex // protect concurrent access

	nextUserID  int64 //globalen id za userje da nastavim naslednjemu
	nextTopicID int64 //ist za topic
	nextMsgID   int64 //ist za msg
	//v go je edina omejitev za dolzino tvoj spomin tko da chillamo
	users    map[int64]*pb.User
	topics   map[int64]*pb.Topic
	messages map[int64][]*pb.Message

	subscribers map[int64][]pb.MessageBoard_SubscribeTopicServer // topicID → streams
}

func newServer() *messageBoardServer {
	return &messageBoardServer{
		users:       make(map[int64]*pb.User),
		topics:      make(map[int64]*pb.Topic),
		messages:    make(map[int64][]*pb.Message),
		subscribers: make(map[int64][]pb.MessageBoard_SubscribeTopicServer),
	}
}

// rpc CreateUser(CreateUserRequest) returns (User);
func (s *messageBoardServer) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.User, error) {
	//ne pozabit locke ka do zdej si jih povsot jih rabis
	s.mu.Lock()
	defer s.mu.Unlock()

	//povecamo stevilo userjev
	s.nextUserID++
	//naredimo userja iz proto fila po njegovi strukturi z id in name
	user := &pb.User{
		Id:   s.nextUserID,
		Name: req.GetName(),
	}
	s.users[user.Id] = user
	return user, nil
}

// rpc CreateTopic(CreateTopicRequest) returns (Topic);
func (s *messageBoardServer) CreateTopic(ctx context.Context, req *pb.CreateTopicRequest) (*pb.Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	//povecamo stevilo topicov na serverju
	s.nextTopicID++

	//again id in name mogoce bi blo fajn da vem vse message pod topic ali je zadosti ce pod Message dam topic??? pac morda za iskanje lepsi tle spodi???
	topic := &pb.Topic{
		Id: s.nextTopicID,
		//GetName je func znotraj
		Name: req.GetName(),
	}
	s.topics[topic.Id] = topic
	return topic, nil
}

// rpc PostMessage(PostMessageRequest) returns (Message);
func (s *messageBoardServer) PostMessage(ctx context.Context, req *pb.PostMessageRequest) (*pb.Message, error) {
	//treba preverit prvo ce obstaja User glede na to da pb.PostMessageRequest ima UserId lahk po tem iscem
	//nuci go if za lepso preglednost lih ka ma to opcijo (spomini na prog v c ka to ni blo dovoljeno ma sm delal)
	s.mu.RLock() //naj pocaka da se posta preden bere
	if _, ok := s.users[req.UserId]; !ok {
		s.mu.RUnlock()
		//returni + Error message(v goju z malo zacetnico brez locil se neki SonarQube pritozuje drgace najbrz kaka navada ali razlog)
		return nil, fmt.Errorf("user does not exist")
	}
	if _, ok := s.topics[req.TopicId]; !ok {
		s.mu.RUnlock()
		//returni + Error message
		return nil, fmt.Errorf("topic does not exist")
	}
	s.mu.RUnlock()

	//Ce po nekem cudezu gremo cez errorje rabimo nrditi post na topic
	s.mu.Lock()
	s.nextMsgID++
	message := &pb.Message{
		Id:        s.nextMsgID,
		TopicId:   req.TopicId,
		UserId:    req.UserId,
		Text:      req.Text,
		CreatedAt: timestamppb.Now(), //to nastavi time na trenutn cajt na serverju type of *timestamppb.Timestamp
		Likes:     0,                 //zcni z 0
	}
	//lahk tud GetTopicId mogoce ne vem zakaj je ta Getter tudi dan najbrz je nek razlog
	s.messages[req.TopicId] = append(s.messages[req.TopicId], message)

	//obvesti narocnike o novi objavi
	event := &pb.MessageEvent{
		SequenceNumber: message.Id,
		Op:             pb.OpType_OP_POST,
		Message:        message,
		EventAt:        timestamppb.Now(),
	}
	subscribers := s.subscribers[req.TopicId]
	s.mu.Unlock()

	// Send to subscribers outside the lock to avoid deadlock
	for _, sub := range subscribers {
		sub.Send(event)
	}
	return message, nil
}

//  rpc DeleteMessage(DeleteMessageRequest) returns (google.protobuf.Empty);

func (s *messageBoardServer) DeleteMessage(ctx context.Context, req *pb.DeleteMessageRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// dubi vse message za ta topic
	messages, ok := s.messages[req.TopicId]
	//ce ni tega topica mu reci naj se gre solit
	if !ok {
		return nil, fmt.Errorf("topic does not exist")
	}

	// isci skozi message
	for i, msg := range messages {
		if msg.Id == req.MessageId {
			// ce user ni owner
			if msg.UserId != req.UserId {
				return nil, fmt.Errorf("user is not the owner of this message")
			}
			// zbrisi message do ija brez ija in od ija naprej zdruzi da nimam praznih vmes pole
			s.messages[req.TopicId] = append(messages[:i], messages[i+1:]...)
			return &emptypb.Empty{}, nil
		}
	}

	// message ne obstaja
	return nil, fmt.Errorf("message with this id does not exist")
}

// rpc LikeMessage(LikeMessageRequest) returns (Message);
func (s *messageBoardServer) LikeMessage(ctx context.Context, req *pb.LikeMessageRequest) (*pb.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages, ok := s.messages[req.TopicId]
	if !ok {
		return nil, fmt.Errorf("topic does not exist")
	}
	for _, msg := range messages {
		if msg.Id == req.MessageId {
			msg.Likes++
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message with this id not found")
}

// ///////////////////////////
// rpc GetSubcscriptionNode(SubscriptionNodeRequest) returns (SubscriptionNodeResponse);
func (s *messageBoardServer) GetSubcscriptionNode(ctx context.Context, req *pb.SubscriptionNodeRequest) (*pb.SubscriptionNodeResponse, error) {
	//za implementirat vec ko bo vec nodov also ne lockat
	return &pb.SubscriptionNodeResponse{
		SubscribeToken: "OK",
		Node: &pb.NodeInfo{
			NodeId:  "main",
			Address: "localhost:50051",
		},
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

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterMessageBoardServer(grpcServer, newServer())

	log.Println("Server running on port 50051...")
	grpcServer.Serve(lis)
}

//za popravit GetSubcscriptionNode
