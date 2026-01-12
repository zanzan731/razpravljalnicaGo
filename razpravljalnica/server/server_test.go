package server

import (
	"context"
	"testing"

	pb "razpravljalnica/proto"

	"google.golang.org/protobuf/types/known/emptypb"
)

// Da nrdim test Server
func newTestServer() *messageBoardServer {
	return &messageBoardServer{
		users:       make(map[int64]*pb.User),
		topics:      make(map[int64]*pb.Topic),
		messages:    make(map[int64][]*pb.Message),
		subscribers: make(map[int64][]pb.MessageBoard_SubscribeTopicServer),
		nextUserID:  0,
		nextTopicID: 1,
		nextMsgID:   1,
		isHead:      true, // za testing najlazje
		isTail:      true, // za testing najlazje
		dirtyOps:    make(map[int64]*dirtyOperation),
		appliedOps:  make(map[int64]bool),
	}
}

/*
// simple za zacet testiri ce se user nrdi, changed by create and register
func TestCreateUser(t *testing.T) {
	s := newTestServer()
	user, err := s.CreateUser(context.Background(), &pb.CreateUserRequest{Name: "TestUser"})

	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	//za vsak slucaj
	if user.Name != "TestUser" {
		t.Errorf("Expected user name 'TestUser', got '%s'", user.Name)
	}

	if user.Id != 1 {
		t.Errorf("Expected user ID 1, got %d", user.Id)
	}

	// poglej da je user stored na serverju
	s.mu.RLock()
	storedUser, exists := s.users[user.Id]
	s.mu.RUnlock()

	if !exists {
		t.Error("User was not saved on server")
	}
	//ali se shrani pod pravim imenom (sam dodajam reci by this point also ce so presledki mi bo to failallo najbrz sm ze pozabu iskren ma mislim da cuttam al je blo to na clientu idk)
	if storedUser.Name != "TestUser" {
		t.Errorf("Stored user name mismatch: got '%s'", storedUser.Name)
	}
}
*/
// Nov RegisterUser
func TestRegisterUser(t *testing.T) {
	s := newTestServer()
	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{
		Username: "testuser",
		Password: "password123",
	})

	if err != nil {
		t.Fatalf("RegisterUser failed: %v", err)
	}

	if user.Name != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", user.Name)
	}

	if user.Id != 1 {
		t.Errorf("Expected user ID 1, got %d", user.Id)
	}

	// Check that password is stored (hashed)
	s.mu.RLock()
	storedUser, exists := s.users[user.Id]
	s.mu.RUnlock()

	if !exists {
		t.Error("User was not saved on server")
	}

	if storedUser.Password == "password123" {
		t.Error("Password should be hashed, not stored as plaintext")
	}
}

// Login
func TestLoginUser(t *testing.T) {
	s := newTestServer()
	// registriraj
	_, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{
		Username: "logintest",
		Password: "correctpass",
	})
	if err != nil {
		t.Fatalf("RegisterUser failed: %v", err)
	}

	// Z pravimi podatki
	user, err := s.LoginUser(context.Background(), &pb.LoginRequest{
		Username: "logintest",
		Password: "correctpass",
	})

	if err != nil {
		t.Fatalf("LoginUser with correct credentials failed: %v", err)
	}

	if user.Name != "logintest" {
		t.Errorf("Expected username 'logintest', got '%s'", user.Name)
	}

	// Check that password is NOT returned
	if user.Password != "" {
		t.Error("Password should not be returned on login")
	}
}

// Login z napacnim passwordom
func TestLoginUserInvalidPassword(t *testing.T) {
	s := newTestServer()
	// registriraj
	_, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{
		Username: "logintest",
		Password: "correctpass",
	})
	if err != nil {
		t.Fatalf("RegisterUser failed: %v", err)
	}

	// Try to login with wrong password
	_, err = s.LoginUser(context.Background(), &pb.LoginRequest{
		Username: "logintest",
		Password: "wrongpass",
	})

	if err == nil {
		t.Error("Expected error with wrong password, got nil")
	}
}

// Neprav user
func TestLoginUserNotFound(t *testing.T) {
	s := newTestServer()
	_, err := s.LoginUser(context.Background(), &pb.LoginRequest{
		Username: "nonexistent",
		Password: "password",
	})

	if err == nil {
		t.Error("Expected error for nonexistent user, got nil")
	}
}

// Isti username
func TestRegisterUserDuplicate(t *testing.T) {
	s := newTestServer()
	_, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{
		Username: "duplicate",
		Password: "pass1",
	})
	if err != nil {
		t.Fatalf("First RegisterUser failed: %v", err)
	}

	// Try to register again with same username
	_, err = s.RegisterUser(context.Background(), &pb.RegisterRequest{
		Username: "duplicate",
		Password: "pass2",
	})

	if err == nil {
		t.Error("Expected error when registering duplicate username, got nil")
	}
}

// fuzz za RegisterUser z random usernames in passwords
func FuzzRegisterUser(f *testing.F) {
	// Neki primerov za zacetk
	f.Add("Alice", "password123")
	f.Add("Bob123", "secure_pass")
	f.Add("User_with_underscores", "pass_123")
	f.Add("Very Long Username With Spaces", "longpassword")
	f.Add("", "password")
	f.Add("愛", "日本語パス") // unicode test

	f.Fuzz(func(t *testing.T, username, password string) {
		s := newTestServer()

		// Register user
		user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{
			Username: username,
			Password: password,
		})

		// Ne sme crashat tud ce je username invalid
		if err != nil {
			// Ce ima error je okej, sam ne sme crashat
			return
		}

		// Ce je success, checkiraj da so podatki smiselni
		if user == nil {
			t.Fatal("User was nil but no error")
		}

		if user.Name != username {
			t.Errorf("Username mismatch: expected '%s', got '%s'", username, user.Name)
		}

		if user.Id <= 0 {
			t.Errorf("Invalid user ID: %d", user.Id)
		}

		// Preveri da je shranjen in password je hashiran
		s.mu.RLock()
		storedUser, exists := s.users[user.Id]
		s.mu.RUnlock()

		if !exists {
			t.Error("User not saved on server")
		}

		if storedUser.Name != username {
			t.Errorf("Stored username mismatch: expected '%s', got '%s'", username, storedUser.Name)
		}

		// Password should be hashed, not plaintext
		if storedUser.Password == password {
			t.Error("Password should be hashed, not stored as plaintext")
		}

		// Try to login with correct password
		loginUser, err := s.LoginUser(context.Background(), &pb.LoginRequest{
			Username: username,
			Password: password,
		})

		if err != nil {
			t.Errorf("Login failed with correct password: %v", err)
			return
		}

		if loginUser.Name != username {
			t.Errorf("Login returned wrong username: expected '%s', got '%s'", username, loginUser.Name)
		}

		// Password should not be returned on login
		if loginUser.Password != "" {
			t.Error("Password should not be returned on login")
		}
	})
}

func TestCreateTopic(t *testing.T) {
	s := newTestServer()
	//mogoce bi blo fajn da dejansko preverjamo da je valid user creiral topic drgac ma unlucky sm zdej ugotovu da nas boli k kdo nrdi topic ko sm pisal test
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test topic"})

	if err != nil {
		t.Fatalf("CreateTopic failed: %v", err)
	}

	if topic.Name != "Test topic" {
		t.Errorf("Expected topic name 'Test topic', got '%s'", topic.Name)
	}

	// A je stored topic?
	s.mu.RLock()
	_, exists := s.topics[topic.Id]
	s.mu.RUnlock()

	if !exists {
		t.Error("Topic was not stored in server")
	}
}

func TestListTopics(t *testing.T) {
	s := newTestServer()

	topicNames := []string{"Topic1", "Topic2", "Topic3"}
	for _, name := range topicNames {
		s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: name})
	}

	// Get all topics
	resp, err := s.ListTopics(context.Background(), &emptypb.Empty{})

	if err != nil {
		t.Fatalf("GetTopics failed: %v", err)
	}

	if len(resp.Topics) != len(topicNames) {
		t.Errorf("Expected %d topics, got %d", len(topicNames), len(resp.Topics))
	}
	//ne bom gledal se po imenih gledam v CreateTopics vem kaj dela...
}

func TestPostMessage(t *testing.T) {
	s := newTestServer()

	// Nrdi userja in topic
	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestPostMessage there was a problem with RegisterUser for some fucking reason....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestPostMessage there was a problem with CreateTopic for some fucking reason....????")
	}
	// Message
	req := &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user.Id,
		Text:    "Banger project, ma komu se da pisat teste like... ceprov morem priznat to ni dost drugace ku pisat navadno kodo tko da go dobi tle plus za pisanje testov (ne za dejansko ku se zaganja teste in vse ma sam za pisanje)",
	}
	msg, err := s.PostMessage(context.Background(), req)

	if err != nil {
		t.Fatalf("PostMessage failed: %v", err)
	}

	if msg.Text != "Banger project, ma komu se da pisat teste like... ceprov morem priznat to ni dost drugace ku pisat navadno kodo tko da go dobi tle plus za pisanje testov (ne za dejansko ku se zaganja teste in vse ma sam za pisanje)" {
		t.Errorf("Got text '%s' wich was not expected", msg.Text)
	}

	if msg.UserId != user.Id {
		t.Errorf("Expected user ID %d, got %d", user.Id, msg.UserId)
	}

	// A je msg stored
	s.mu.RLock()
	messages := s.messages[topic.Id]
	s.mu.RUnlock()

	if len(messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(messages))
	}
}

func TestGetMessages(t *testing.T) {
	s := newTestServer()
	// User in topic prvo
	user, _ := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	topic, _ := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	// pole message
	messageTexts := []string{"Message 1", "Message 2", "Message 3"}
	for _, text := range messageTexts {
		s.PostMessage(context.Background(), &pb.PostMessageRequest{TopicId: topic.Id, UserId: user.Id, Text: text})
	}

	// Get messages
	resp, err := s.GetMessages(context.Background(), &pb.GetMessagesRequest{TopicId: topic.Id})

	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	//v proto pise repeated tko da to je seznam navadn in najbrz bo slo tko kr
	if len(resp.Messages) != len(messageTexts) {
		t.Errorf("Expected %d messages, got %d", len(messageTexts), len(resp.Messages))
	}

	// Preglej se order messagov ne vem ce je zares nujn da mi vraca po vrsti ma je fajn da bi ce ne vraca po vrsti to vrzem vn me ne zadost briga sam me zanima
	for i, msg := range resp.Messages {
		if msg.Text != messageTexts[i] {
			t.Errorf("Message %d: expected '%s', got '%s'", i, messageTexts[i], msg.Text)
		}
	}
}

func TestUpdateMessage(t *testing.T) {
	s := newTestServer()
	// Create user + topics + messages
	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestUpdateMessage there was a problem with RegisterUser for some fucking reason....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestUpdateMessage there was a problem with CreateTopic for some fucking reason....????")
	}
	msg, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user.Id,
		Text:    "Original text",
	})
	if err != nil {
		t.Fatalf("In TestUpdateMessage there was a problem with PostMessage for some fucking reason....????")
	}

	// Update the message
	updatedMsg, err := s.UpdateMessage(context.Background(), &pb.UpdateMessageRequest{
		MessageId: msg.Id,
		Text:      "Updated text",
		UserId:    user.Id,
		TopicId:   topic.Id,
	})

	if err != nil {
		t.Fatalf("UpdateMessage failed: %v", err)
	}

	if updatedMsg.Text != "Updated text" {
		t.Errorf("Expected updated text 'Updated text', got '%s'", updatedMsg.Text)
	}

	// A je message updated v storage-u
	s.mu.RLock()
	storedMsg := s.messages[topic.Id][0]
	s.mu.RUnlock()

	if storedMsg.Text != "Updated text" {
		t.Errorf("Stored message not updated: got '%s' (expected 'Updated text')", storedMsg.Text)
	}
}

func TestDeleteMessage(t *testing.T) {
	s := newTestServer()

	// user + topics + msg post
	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestDeleteMessage there was a problem with RegisterUser for some fucking reason....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestDeleteMessage there was a problem with CreateTopic for some fucking reason....????")
	}
	msg1, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{TopicId: topic.Id, UserId: user.Id, Text: "Message 1"})
	if err != nil {
		t.Fatalf("In TestDeleteMessage there was a problem with PostMessage 1 for some fucking reason....????")
	}
	msg2, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{TopicId: topic.Id, UserId: user.Id, Text: "Message 2"})
	if err != nil {
		t.Fatalf("In TestDeleteMessage there was a problem with PostMessage 2 ce se pa to zgodi nrdim rope chair combo ka the fuck....????")
	}

	// Delete the first message
	// Ce koga zanima zakaj = namesto := je zato ka go ne pusti := tega ce ni novih variablov na levi kar pac fer ma sm nekako mislu da un _ bo idk ful me je zatripal to za nek razlog kljub temu da ma smisu pomojem zato ka sm ze cel dan za kompom in programiram ze 5 ur ma pustmo stat
	_, err = s.DeleteMessage(context.Background(), &pb.DeleteMessageRequest{
		TopicId:   topic.Id,
		MessageId: msg1.Id,
		UserId:    user.Id,
	})

	if err != nil {
		t.Fatalf("DeleteMessage failed: %v", err)
	}

	// Za preverit da se je res zbrisal v storage...
	s.mu.RLock()
	messages := s.messages[topic.Id]
	s.mu.RUnlock()

	//ce koga zanima ja vecina komentarjov mi chatko sam predlaga in priznam da je banger ka komu se da to pisat
	if len(messages) != 1 {
		t.Errorf("Expected 1 message after deletion, got %d", len(messages))
	}

	if messages[0].Id != msg2.Id {
		t.Errorf("Wrong message deleted: expected ID %d, got %d", msg2.Id, messages[0].Id)
	}
}

// taki neki robni primeri
func TestPostMessageInvalidTopic(t *testing.T) {
	s := newTestServer()

	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestPostMessageInvalidTopic there was a problem with RegisterUser for some fucking reason....????")
	}
	// Provi postat v nek topic ki ne obstaja
	_, err = s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: 929849532150132525, //idk to je se sigurn v int64
		UserId:  user.Id,
		Text:    "Test",
	})

	if err == nil {
		t.Error("Expected error when posting to invalid topic, got nil")
	}
}

// za uni update ce dela unauthorize oseba
func TestUpdateMessageUnauthorized(t *testing.T) {
	s := newTestServer()
	// Create two users
	user1, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with RegisterUser 1 for some fucking reason....????")
	}
	user2, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "Bob", Password: "pass456"})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with RegisterUser 2 for some fucking reason chair and rope combo....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with CreateTopic for some fucking reason....????")
	}
	// User1 posts a message
	msg, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user1.Id,
		Text:    "Original text",
	})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with PostMessage for some fucking reason....????")
	}
	// User2 tries to update it (should fail)
	_, err = s.UpdateMessage(context.Background(), &pb.UpdateMessageRequest{
		MessageId: msg.Id,
		Text:      "Am buljs zate da ne updajtas",
		UserId:    user2.Id,
	})

	if err == nil {
		t.Error("Expected error when unauthorized user tries to update message, got nil")
	}
}

// Ist sam za delete
func TestDeleteMessageUnauthorized(t *testing.T) {
	s := newTestServer()
	// Create two users
	user1, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestDeleteMessageUnauthorized there was a problem with RegisterUser 1 for some fucking reason....????")
	}
	user2, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "Bob", Password: "pass456"})
	if err != nil {
		t.Fatalf("In TestDeleteMessageUnauthorized there was a problem with RegisterUser 2 for some fucking reason chair and rope combo....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with CreateTopic for some fucking reason....????")
	}
	// User1 posts a message
	msg, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user1.Id,
		Text:    "Original text",
	})
	if err != nil {
		t.Fatalf("In TestUpdateMessageUnauthorized there was a problem with PostMessage for some fucking reason....????")
	}
	// User2 tries to delete it (should fail)
	_, err = s.DeleteMessage(context.Background(), &pb.DeleteMessageRequest{
		MessageId: msg.Id,
		UserId:    user2.Id,
		TopicId:   topic.Id,
	})

	if err == nil {
		t.Error("Expected error when unauthorized user tries to delete message, got nil")
	}
}

// pozabu like message moja
func TestLikeMessage(t *testing.T) {
	s := newTestServer()
	// Create two users
	user1, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestLikeMessage there was a problem with RegisterUser 1 for some fucking reason....????")
	}
	user2, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "Bob", Password: "pass456"})
	if err != nil {
		t.Fatalf("In TestLikeMessage there was a problem with RegisterUser 2 for some fucking reason chair and rope combo....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestLikeMessage there was a problem with CreateTopic for some fucking reason....????")
	}
	// User1 posts a message
	msg, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user1.Id,
		Text:    "Original text",
	})
	if err != nil {
		t.Fatalf("In TestLikeMessage there was a problem with PostMessage for some fucking reason....????")
	}
	//User 2 likes a message
	_, err = s.LikeMessage(context.Background(), &pb.LikeMessageRequest{TopicId: topic.Id, UserId: user2.Id, MessageId: msg.Id})
	if err != nil {
		t.Fatalf("There was a problem liking a message")
	}
}

// basicly ce sam lika
func TestLikeMessageUnauthorized(t *testing.T) {
	s := newTestServer()
	// Create two users
	user, err := s.RegisterUser(context.Background(), &pb.RegisterRequest{Username: "zanzan", Password: "pass123"})
	if err != nil {
		t.Fatalf("In TestLikeMessageUnauthorized there was a problem with RegisterUser for some fucking reason....????")
	}
	topic, err := s.CreateTopic(context.Background(), &pb.CreateTopicRequest{Name: "Test Topic"})
	if err != nil {
		t.Fatalf("In TestLikeMessageUnauthorized there was a problem with CreateTopic for some fucking reason....????")
	}
	// User1 posts a message
	msg, err := s.PostMessage(context.Background(), &pb.PostMessageRequest{
		TopicId: topic.Id,
		UserId:  user.Id,
		Text:    "Original text",
	})
	if err != nil {
		t.Fatalf("In TestLikeMessageUnauthorized there was a problem with PostMessage for some fucking reason....????")
	}
	//User 2 likes a message
	_, err = s.LikeMessage(context.Background(), &pb.LikeMessageRequest{TopicId: topic.Id, UserId: user.Id, MessageId: msg.Id})
	if err == nil {
		t.Fatalf("The user who posted message could also like it")
	}
}

//ne vem ali zelijo da tudi repikacijo in vse testiram ma iskreno ne vem kako bi to nrdu ka mam uni isInternal al karkol ka dodajam podpis uno isHead isTail ne mislim testirat se mi zdi glupo...
//subscribe tud ne vem kako testirat pac neki bi mogu cakat na stream idk??
