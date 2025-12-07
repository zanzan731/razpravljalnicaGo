package main

import (
	"log"

	pb "razpravljalnica/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	// Connect
	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("connection failed:", err)
	}
	defer conn.Close()

	client := pb.NewMessageBoardClient(conn)

}
