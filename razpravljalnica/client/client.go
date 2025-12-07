package main

import (
	"bufio"
	"context"
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
	reader := bufio.NewReader(os.Stdin)
	// Connect
	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("connection failed:", err)
	}
	defer conn.Close()

	client := pb.NewMessageBoardClient(conn)
	fmt.Println("Connected to server!")

	fmt.Print("Enter username:")
	//obstaja tud scanln sam se splaca pomojem bolj tko
	username, _ := reader.ReadString('\n')

	//Sm ponesreci prej dal pred enter username in mi je crashavalo in nism vedu zakaj lol :/
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	user, err := client.CreateUser(ctx, &pb.CreateUserRequest{Name: username})
	if err != nil {
		log.Fatal("Faled creating user:", err)
	}
	fmt.Println("Logged in as user: ", user.Name, "ID: ", user.Id)

	//Interactive Shell
	runShell(client, user.Id)
}

func runShell(c pb.MessageBoardClient, userID int64) {
	//nov reader ka je nova funkcija pomojem se ne da unega od gori nucat
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("")
	fmt.Println("Commands:")
	fmt.Println("   topics             - list topics")
	fmt.Println("   newtopic NAME      - create topic")
	fmt.Println("   sub ID             - subscribe to topic")
	fmt.Println("   send ID MESSAGE    - send message")
	fmt.Println("   msgs ID            - list messages in topic")
	fmt.Println("   help               - show commands")
	fmt.Println("   exit               - quit")
	fmt.Println("--------------------------------------------------")
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
			resp, err := c.ListTopics(ctx, &emptypb.Empty{})
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
				fmt.Printf("Usage: newtopic <topic-name>")
				continue
			}
			//Ime more bit single string tko da vrni nazaj v prvotno ce si delil prej ko si razdelil po presledkih (ce je zelel vec presledkov pa se lahk gre kr solit ka kdo bi to nrdu)
			name := strings.Join(args[1:], " ")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			topic, err := c.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name})
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Created topic: [%d] %s\n", topic.Id, topic.Name)
		///TODO implementiraj pole ka je stream
		case "sub":
			if len(args) < 2 {
				fmt.Println("Usage: sub <topic-id>")
			}
			topicID := toInt64(args[1])
			if topicID <= 0 {
				fmt.Printf("TopicID must be a number bigger than 0.\nUsage: sub <topic-id>")
				continue
			}
			fmt.Println("Subscribing to topic", topicID, "…")
			go subscribeLoop(c, topicID)
		case "send":
			if len(args) < 3 {
				fmt.Println("Usage: send <topic-id> <message>")
			}
			textMessage := strings.Join(args[2:], " ")
			topicID := toInt64(args[1])
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			message, err := c.PostMessage(ctx, &pb.PostMessageRequest{TopicId: topicID, Text: textMessage, UserId: userID})
			cancel()
			if err != nil {
				fmt.Println("Error: ", err)
				continue
			}
			fmt.Printf("Posted message on topic %d: %s\n", message.TopicId, message.Text)
		//uni GetMessages, vrne vse message znotraj toppica
		case "msgs":
			if len(args) != 2 {
				fmt.Println("Usage: msgs <topic-id>")
			}
			topicID := toInt64(args[1])
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			messages, err := c.GetMessages(ctx, &pb.GetMessagesRequest{TopicId: topicID})
			cancel()
			if err != nil {
				fmt.Println("Error", err)
				continue
			}
			fmt.Println("All messages for topic ", topicID, ":")
			for _, message := range messages.Messages {
				fmt.Printf("[%d]: %s\n", message.UserId, message.Text)
			}
		case "help":
			fmt.Println("")
			fmt.Println("Commands:")
			fmt.Println("   topics             - list topics")
			fmt.Println("   newtopic NAME      - create topic")
			fmt.Println("   sub ID             - subscribe to topic")
			fmt.Println("   send ID MESSAGE    - send message")
			fmt.Println("   msgs ID            - list messages in topic")
			fmt.Println("   help               - show commands")
			fmt.Println("   exit               - quit")
			fmt.Println("--------------------------------------------------")
		case "exit":
			fmt.Println("Good bye!")
			return
		default:
			fmt.Println("Unknown command. Type 'help'.")

		}
	}
}
func subscribeLoop(c pb.MessageBoardClient, topicID int64) {
	stream, err := c.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{TopicId: []int64{topicID}})
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
