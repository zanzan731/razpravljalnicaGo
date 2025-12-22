package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	pb "razpravljalnica/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

func main() {
	controlPlaneFlag := flag.String("control-plane", "localhost:6000", "control plane address")
	flag.Parse()

	reader := bufio.NewReader(os.Stdin)

	// Dubi head in tail iz control plane
	cpConn, err := grpc.NewClient(*controlPlaneFlag, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("control-plane connection failed:", err)
	}
	defer cpConn.Close() //da zpre conection predn se konca na koncu bi pozabu drgace, zdej a je defer slabsi ku ce napisem na koncu upam da to compiler pole prov nrdi ka jaz necem razmisljat o tem
	cp := pb.NewControlPlaneClient(cpConn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	state, err := cp.GetClusterState(ctx, &emptypb.Empty{}) //head tail in chain
	cancel()                                                //nismo dobili odgovora od control plane
	if err != nil {
		log.Fatal("failed to get cluster state:", err) //neki sfukan state na control plane
	}

	headAddr := state.GetHead().GetAddress() //kje je head
	tailAddr := state.GetTail().GetAddress() //kje je tail
	//////////////////////////////////// CONNECTION TO HEAD /////////////////////////////////////////////////////////////////
	headConn, err := grpc.NewClient(headAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("head connection failed:", err)
	}
	//////////////////////////////////// CONNECTION TO TAIL /////////////////////////////////////////////////////////////////
	var tailConn *grpc.ClientConn
	///////////sam en server ne se dvakrat povezovat na isti port ka bos nrdu lahk sam probleme sploh ce ni pravih checkov....
	if tailAddr == headAddr {
		tailConn = headConn
	} else {
		//connect to tail
		tailConn, err = grpc.NewClient(tailAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			headConn.Close()
			log.Fatal("tail connection failed:", err)
		}
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	headClient := pb.NewMessageBoardClient(headConn)
	tailClient := pb.NewMessageBoardClient(tailConn)
	//pole vrzi vn zaenkrat pusti da vidis kam si connectan
	fmt.Printf("Connected. head=%s tail=%s\n", headAddr, tailAddr)

	///zacetek logina
	fmt.Print("Enter username:")
	//obstaja tud scanln
	username, _ := reader.ReadString('\n') //bere do newline lahk das kirkol znak drgac kr doro

	//Sm ponesreci prej dal pred enter username in mi je crashavalo in nism vedu zakaj lol :/
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	user, err := headClient.CreateUser(ctx, &pb.CreateUserRequest{Name: username})
	cancel()
	if err != nil {
		//zpri vse prej
		headConn.Close()
		if tailConn != nil && tailConn != headConn {
			tailConn.Close()
		}
		log.Fatal("failed creating user:", err)
	}
	fmt.Println("Logged in as user: ", user.Name, "ID: ", user.Id)
	//Interactive Shell -- po novem rabi vse podatke o tem komu posiljat kaj... treba cekirat tud na serverju vse da se na pravih krajih bere in pise
	runShell(cp, headClient, tailClient, user.Id)
	//zpri se head in tail connection
	headConn.Close()
	//spomni se od prej lahk sta ista ne mores zprt ze zaprte povezave
	if tailConn != nil && tailConn != headConn {
		tailConn.Close()
	}
}

func runShell(cp pb.ControlPlaneClient, head pb.MessageBoardClient, tail pb.MessageBoardClient, userID int64) {
	//nov reader ka je nova funkcija pomojem se ne da unega od gori nucat
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("")
	fmt.Println("Commands:")
	fmt.Println("   topics                          - list topics")
	fmt.Println("   newtopic NAME                   - create topic")
	fmt.Println("   sub ID                          - subscribe to topic")
	fmt.Println("   send ID MESSAGE                 - send message")
	fmt.Println("   update TOPICID MSGID MESSAGE    - update message")
	fmt.Println("   msgs ID                         - list messages in topic")
	fmt.Println("   del TOPICID MSGID               - delete your message")
	fmt.Println("   like TOPICID MSGID              - like message")
	fmt.Println("   help                            - show commands")
	fmt.Println("   exit                            - quit")
	fmt.Println("-------------------------------------------------------------")
	//endless loop dokler ne rece da ce vn je prjavljen ist ku for true ocitn SonarQube reku da se tko pise v goju
	for {
		//nrdi lep user interface da zgleda ku neki kar bi se dal uporabljat
		fmt.Print(">>> ")
		//preberi command
		line, _ := reader.ReadString('\n')
		//Trim space vrze vse presledke spredi in zadi vn da se ne zajebavamo also strings ma puhno commandov pomojem karkol kar rabis je tle not
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}
		//go back to C :), Split nrdi seznam splita po presledkih
		args := strings.Split(line, " ")
		//sam ce user ne zna delat s space-i lahk bi reku ko ga jebe ma naj mu bo
		for i, str := range args {
			args[i] = strings.TrimSpace(str)
		}

		//command je prvi string v tabeli stringov args
		cmd := args[0]
		switch cmd {
		//List all topics on server
		case "topics":
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			resp, err := tail.ListTopics(ctx, &emptypb.Empty{}) //klici tail za podatke -- reads tail! (sprememba iz prej ka je bil c. ka je itak bil sam en client)
			cancel()
			if err != nil {
				fmt.Println("Error", err)
				continue
			}
			if len(resp.Topics) == 0 {
				fmt.Println("There are no open topics yet!")
				continue
			}
			for _, t := range resp.Topics {
				fmt.Printf("[%d] %s\n", t.Id, t.Name)
			}
		//Nrdi nov topic na serverju
		case "newtopic":
			if len(args) < 2 {
				fmt.Println("Usage: newtopic <topic-name>")
				continue
			}
			//Ime more bit single string tko da vrni nazaj v prvotno ce si delil prej ko si razdelil po presledkih (ce je zelel vec presledkov pa se lahk gre kr solit ka kdo bi to nrdu) --pozabi dela ka itak je seznam in zdruzujem s tem da dodam presledk in mam pole pac seznam daljsi ma so umes prazni ki jih zapomnem z presledki
			name := strings.Join(args[1:], " ")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			topic, err := head.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name})
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Created topic: [%d] %s\n", topic.Id, topic.Name)
		//Subscribe to topic
		case "sub":
			if len(args) < 2 {
				fmt.Println("Usage: sub <topic-id>")
			}
			topicID := toInt64(args[1])
			if topicID <= 0 {
				fmt.Printf("TopicID must be a number bigger than 0.\nUsage: sub <topic-id>\n")
				continue
			}
			fmt.Println("Subscribing to topic", topicID, "…")
			go subscribeLoop(cp, topicID, userID) //subscribamo se na node ki nam ga dodeli
		case "send":
			if len(args) < 3 {
				fmt.Println("Usage: send <topic-id> <message>")
			}
			textMessage := strings.Join(args[2:], " ")
			topicID := toInt64(args[1])
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			message, err := head.PostMessage(ctx, &pb.PostMessageRequest{TopicId: topicID, Text: textMessage, UserId: userID}) //send always to the head
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Posted message on topic %d: %s\n", message.TopicId, message.Text)
		case "update":
			if len(args) < 4 {
				fmt.Println("Usage: update <topic-id> <msg-id> <message>")
			}
			topicID := toInt64(args[1])
			msgID := toInt64(args[2])
			textMessage := strings.Join(args[3:], " ")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			message, err := head.UpdateMessage(ctx, &pb.UpdateMessageRequest{TopicId: topicID, MessageId: msgID, UserId: userID, Text: textMessage}) // update always to the head
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Updated message %d on topic %d: %s\n", message.Id, message.TopicId, message.Text)

			//uni GetMessages, vrne vse message znotraj toppica
		case "msgs":
			if len(args) != 2 {
				fmt.Println("Usage: msgs <topic-id>")
			}
			topicID := toInt64(args[1])
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			messages, err := tail.GetMessages(ctx, &pb.GetMessagesRequest{TopicId: topicID}) //list all messages always to tail
			cancel()
			if err != nil {
				fmt.Println("Error", err)
				continue
			}
			fmt.Println("All messages for topic ", topicID, ":")
			for _, message := range messages.Messages {
				fmt.Printf("[%d, %d]: %s      Likes: %d\n", message.Id, message.UserId, message.Text, message.Likes)
			}
		case "del":
			if len(args) != 3 {
				fmt.Println("Usage: del <topic-id> <msg-id>")
			}
			topicID := toInt64(args[1])
			msgID := toInt64(args[2])

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := head.DeleteMessage(ctx, &pb.DeleteMessageRequest{MessageId: msgID, UserId: userID, TopicId: topicID}) //del always to head
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Message %d succesfully deleted from topic %d\n", msgID, topicID)
		case "like":
			if len(args) != 3 {
				fmt.Println("Usage: like <topic-id> <msg-id>")
			}
			topicID := toInt64(args[1])
			msgID := toInt64(args[2])

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			message, err := head.LikeMessage(ctx, &pb.LikeMessageRequest{MessageId: msgID, UserId: userID, TopicId: topicID}) // like always to head
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("You liked msg: %d\n", message.Id)

		case "help":
			fmt.Println("")
			fmt.Println("Commands:")
			fmt.Println("   topics                          - list topics")
			fmt.Println("   newtopic NAME                   - create topic")
			fmt.Println("   sub ID                          - subscribe to topic")
			fmt.Println("   send ID MESSAGE                 - send message")
			fmt.Println("   update TOPICID MSGID MESSAGE    - update message")
			fmt.Println("   msgs ID                         - list messages in topic")
			fmt.Println("   del TOPICID MSGID               - delete your message")
			fmt.Println("   like TOPICID MSGID              - like message")
			fmt.Println("   help                            - show commands")
			fmt.Println("   exit                            - quit")
			fmt.Println("-------------------------------------------------------------")
		case "exit":
			fmt.Println("Good bye!")
			return
		default:
			fmt.Println("Unknown command. Type 'help'.")

		}
	}
}
func subscribeLoop(cp pb.ControlPlaneClient, topicID int64, userID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	resp, err := cp.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: userID, TopicId: []int64{topicID}})
	cancel()
	if err != nil {
		fmt.Println("Subscription node lookup failed:", err)
		return
	}

	addr := resp.GetNode().GetAddress()
	subConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("Connect to subscription node failed:", err)
		return
	}
	defer subConn.Close()

	subClient := pb.NewMessageBoardClient(subConn)
	stream, err := subClient.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		TopicId:        []int64{topicID},
		UserId:         userID,
		SubscribeToken: resp.GetSubscribeToken(),
	})
	if err != nil {
		fmt.Println("Subscribe failed:", err)
		return
	}

	for {
		//Recv() sprejme naslednji response from server
		event, err := stream.Recv()
		//EOF se poslje na koncu ko se streem terminata
		if err == io.EOF {
			fmt.Println("Stream closed for topic", topicID)
			return
		}
		if err != nil {
			fmt.Println("Stream error:", err)
			return
		}
		fmt.Printf("\n NEW MESSAGE IN TOPIC %d: %s\n>>> ", topicID, event.Message.Text)
	}
}
func toInt64(s string) int64 {
	var x int64
	fmt.Sscan(s, &x)
	return x
}
