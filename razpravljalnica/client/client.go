package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	pb "razpravljalnica/proto"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// za UI
type UI struct {
	//ni lih prov da dam tle not userId ma da ne pisem se vec kode
	UserId int64

	App   *tview.Application
	Pages *tview.Pages

	Menu                     *tview.List
	TopicsView               *tview.List
	MessagesView             *tview.List
	MessagesUpdateDeleteView *tview.List
	SubscriptionView         *tview.List
	NewTopicForm             *tview.Form
	SendMessageTopicsView    *tview.List
	SendMessageForm          *tview.Form
	MessageEditForm          *tview.Form
	UpdateDeleteView         *tview.List
	SelectedTopicID          int64
	CurrentMessageID         int64
	MessageInput             *tview.InputField
	NotificationBar          *tview.TextView

	CP   pb.ControlPlaneClient
	Head pb.MessageBoardClient
	Tail pb.MessageBoardClient
}

func main() {
	controlPlaneFlag := flag.String("control-plane", "localhost:6000", "control plane address")
	flag.Parse()

	// Dubi head in tail iz control plane
	cpConn, err := grpc.NewClient(*controlPlaneFlag, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("control-plane connection failed:", err)
	}
	defer cpConn.Close() //da zpre conection predn se konca na koncu bi pozabu drgace, zdej a je defer slabsi ku ce napisem na koncu upam da to compiler pole prov nrdi ka jaz necem razmisljat o tem
	cp := pb.NewControlPlaneClient(cpConn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

	headConn, tailConn := getHeadTailConn(cp, ctx, cancel)
	headClient := pb.NewMessageBoardClient(headConn)
	tailClient := pb.NewMessageBoardClient(tailConn)

	app := tview.NewApplication()
	// zacetek logina
	var username string
	input := tview.NewInputField().
		SetLabel("Enter username:").
		SetFieldWidth(20)
	form := tview.NewForm().
		AddFormItem(input).
		AddButton("OK", func() {
			username = input.GetText()
			app.Stop()
		})
	if err := app.SetRoot(form, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}
	username = strings.TrimSpace(username)

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

func getHeadTailConn(cp pb.ControlPlaneClient, ctx context.Context, cancel context.CancelFunc) (*grpc.ClientConn, *grpc.ClientConn) {
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
	//pole vrzi vn zaenkrat pusti da vidis kam si connectan
	fmt.Printf("Connected. head=%s tail=%s\n", headAddr, tailAddr)
	return headConn, tailConn
}

func runShell(cp pb.ControlPlaneClient, head pb.MessageBoardClient, tail pb.MessageBoardClient, userID int64) {
	app := tview.NewApplication()
	_ = NewUI(app, cp, head, tail, userID)
	if err := app.Run(); err != nil {
		panic(err)
	}
}

func (ui *UI) subscribeLoop(cp pb.ControlPlaneClient, topicID int64, userID int64) {
	ui.SubscriptionView.Clear()
	ui.App.QueueUpdateDraw(func() {
		ui.SubscriptionView.AddItem(fmt.Sprintf("[yellow]Subscribing to topic %d...\n", topicID), "", 0, nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := cp.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: userID, TopicId: []int64{topicID}})
	cancel()
	if err != nil {
		ui.App.QueueUpdateDraw(func() {
			ui.SubscriptionView.AddItem(fmt.Sprintln("[red]Subscription node lookup failed:", err), "", 0, nil)
		})
		return
	}

	addr := resp.GetNode().GetAddress()
	ui.App.QueueUpdateDraw(func() {
		ui.SubscriptionView.AddItem(fmt.Sprintf("[yellow]Connecting to %s...", addr), "", 0, nil)
	})

	subConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		ui.App.QueueUpdateDraw(func() {
			ui.SubscriptionView.AddItem(fmt.Sprintln("[red]Connect to subscription node failed:", err), "", 0, nil)
		})
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
		ui.App.QueueUpdateDraw(func() {
			ui.SubscriptionView.AddItem(fmt.Sprintf("[red]Subscribe failed: %v", err), "", 0, nil)
		})
		return
	}
	ui.App.QueueUpdateDraw(func() {
		ui.SubscriptionView.AddItem(fmt.Sprintf("[green]Subscribed to topic %d", topicID), "", 0, nil)
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Subscribed to topic %d - Waiting for new messages...", topicID))
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})
	for {
		//Recv() sprejme naslednji response from server
		event, err := stream.Recv()
		//EOF se poslje na koncu ko se streem terminata
		if err == io.EOF {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText(fmt.Sprintf("[yellow]Stream closed for topic %d", topicID))
			})
			return
		}
		if err != nil {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText(fmt.Sprintf("[red]Stream error: %v", err))
			})
			return
		}
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText(fmt.Sprintf("[cyan]NEW MSG in topic %d: %s", topicID, event.Message.Text))
		})
	}
}

//////////////////////////////////////////////////////////////////// UI ///////////////////////////////////////////////////////////

func NewUI(app *tview.Application, cp pb.ControlPlaneClient, head pb.MessageBoardClient, tail pb.MessageBoardClient, userId int64) *UI {
	ui := &UI{
		App:    app,
		Pages:  tview.NewPages(),
		CP:     cp,
		Head:   head,
		Tail:   tail,
		UserId: userId,
	}

	ui.NotificationBar = tview.NewTextView().
		SetDynamicColors(true)
	ui.NotificationBar.SetBorder(true).SetTitle(" Notifications ").SetBorderColor(tcell.ColorYellow)
	ui.NotificationBar.SetText("[darkblue]>>> Ready - Subscribe to topics to see new messages here <<<")

	ui.buildMenu()
	ui.buildTopicsPage()
	ui.buildTopicMessagesPage()
	ui.buildSubscriptionPage()
	ui.buildNewTopicPage()
	ui.buildSendMessageTopicsPage()
	ui.buildSendMessageForm()
	ui.buildMessageEditForm()
	ui.buildUpdateDeleteTopics()
	ui.buildMessagesUpdateDeletePage()
	ui.topics()
	ui.subscription()

	mainLayout := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(ui.Pages, 0, 7, true).
		AddItem(ui.NotificationBar, 0, 1, false)

	ui.Pages.AddPage("menu", ui.Menu, true, true)
	ui.Pages.AddPage("topics", ui.TopicsView, true, false)
	ui.Pages.AddPage("messages", ui.MessagesView, true, false)
	ui.Pages.AddPage("subscription", ui.SubscriptionView, true, false)
	ui.Pages.AddPage("newtopic", ui.NewTopicForm, true, false)
	ui.Pages.AddPage("sendmessagetopics", ui.SendMessageTopicsView, true, false)
	ui.Pages.AddPage("sendmessage", ui.SendMessageForm, true, false)
	ui.Pages.AddPage("messageedit", ui.MessageEditForm, true, false)
	ui.Pages.AddPage("update-delete", ui.UpdateDeleteView, true, false)
	ui.Pages.AddPage("messages-update-delete", ui.MessagesUpdateDeleteView, true, false)

	app.SetRoot(mainLayout, true)
	return ui
}

// //////////////////////////////////////////////////////////BUILDING PAGES//////////////////////////////////////////////////////////////
// main page
func (ui *UI) buildMenu() {
	ui.Menu = tview.NewList().
		AddItem("topics", "List all topics", 'a', func() {
			ui.Pages.SwitchToPage("topics")
			ui.topics()
		}).
		AddItem("newtopic NAME", "Creates topic", 'b', func() {
			ui.Pages.SwitchToPage("newtopic")
			ui.App.SetFocus(ui.NewTopicForm)
		}).
		AddItem("sub ID", "Subscribe to topic", 'c', func() {
			ui.Pages.SwitchToPage("subscription")
			ui.subscription()
		}).
		AddItem("send ID MESSAGE", "Send message", 'd', func() {
			ui.Pages.SwitchToPage("sendmessagetopics")
			ui.loadSendMessageTopics()
		}).
		AddItem("update/delete TOPICID MSGID MESSAGE", "update message", 'e', func() {
			ui.Pages.SwitchToPage("update-delete")
			ui.updateDeleteTopics()
		}).
		AddItem("exit", "Quit", 'q', func() {
			ui.App.Stop()
		})
	ui.Menu.SetBorder(true).SetTitle(" Menu ")
}

// Page where i list all the topics
func (ui *UI) buildTopicsPage() {
	ui.TopicsView = tview.NewList()
	ui.TopicsView.SetBorder(true)
	ui.TopicsView.SetTitle(" Topics ")

	ui.TopicsView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})

	ui.TopicsView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
			return nil
		}
		return event
	})
}

// Page where i get all messages when i click on topic
func (ui *UI) buildTopicMessagesPage() {
	ui.MessagesView = tview.NewList()
	ui.MessagesView.SetBorder(true)
	//set title when i know the Topic selected

	ui.MessagesView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("topics")
		ui.App.SetFocus(ui.TopicsView)
	})
	ui.MessagesView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("topics")
			ui.App.SetFocus(ui.TopicsView)
			return nil
		}
		return event
	})

}

// Same as topic but for subscribing to it
func (ui *UI) buildSubscriptionPage() {
	ui.SubscriptionView = tview.NewList()
	ui.SubscriptionView.SetBorder(true)
	ui.SubscriptionView.SetTitle(" Topics for subscription ")

	ui.SubscriptionView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})

	ui.SubscriptionView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
			return nil
		}
		return event
	})
}

func (ui *UI) buildUpdateDeleteTopics() {
	ui.UpdateDeleteView = tview.NewList()
	ui.UpdateDeleteView.SetBorder(true)
	ui.UpdateDeleteView.SetTitle(" Update Delete Topics ")

	ui.UpdateDeleteView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})

	ui.UpdateDeleteView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
			return nil
		}
		return event
	})
}

func (ui *UI) buildMessagesUpdateDeletePage() {
	ui.MessagesUpdateDeleteView = tview.NewList()
	ui.MessagesUpdateDeleteView.SetBorder(true)
	ui.MessagesUpdateDeleteView.SetTitle(" Messages (Update/Delete) ")

	ui.MessagesUpdateDeleteView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("update-delete")
		ui.App.SetFocus(ui.UpdateDeleteView)
	})

	ui.MessagesUpdateDeleteView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("update-delete")
			ui.App.SetFocus(ui.UpdateDeleteView)
			return nil
		}
		return event
	})
}

func (ui *UI) buildNewTopicPage() {
	topicInput := tview.NewInputField().SetLabel("Topic Name:").SetFieldWidth(30)

	ui.NewTopicForm = tview.NewForm().
		AddFormItem(topicInput).
		AddButton("Create", func() {
			topicName := topicInput.GetText()
			if topicName == "" {
				ui.Pages.SwitchToPage("menu")
				ui.App.SetFocus(ui.Menu)
				return
			}
			go ui.newtopic(topicName, topicInput)
		}).
		AddButton("Cancel", func() {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		})

	ui.NewTopicForm.SetBorder(true).SetTitle(" New Topic ")

	ui.NewTopicForm.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
			return nil
		}
		return event
	})

}

func (ui *UI) buildSendMessageTopicsPage() {
	ui.SendMessageTopicsView = tview.NewList()
	ui.SendMessageTopicsView.SetBorder(true)
	ui.SendMessageTopicsView.SetTitle(" Select Topic to Send Message ")

	ui.SendMessageTopicsView.SetDoneFunc(func() {
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})

	ui.SendMessageTopicsView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
			return nil
		}
		return event
	})
}

func (ui *UI) buildSendMessageForm() {
	messageInput := tview.NewInputField().SetLabel("Message:").SetFieldWidth(50)

	ui.SendMessageForm = tview.NewForm().
		AddFormItem(messageInput).
		AddButton("Send", func() {
			messageText := messageInput.GetText()
			if messageText == "" {
				ui.NotificationBar.SetText("[red]Message cannot be empty")
				return
			}
			go ui.sendMessage(messageText, messageInput)
		}).
		AddButton("Cancel", func() {
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		})

	ui.SendMessageForm.SetBorder(true).SetTitle(" Send Message ")

	ui.SendMessageForm.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("sendmessagetopics")
			ui.App.SetFocus(ui.SendMessageTopicsView)
			return nil
		}
		return event
	})
}

func (ui *UI) buildMessageEditForm() {
	ui.MessageInput = tview.NewInputField().SetLabel("Message:").SetFieldWidth(50)

	ui.MessageEditForm = tview.NewForm().
		AddFormItem(ui.MessageInput).
		AddButton("Update", func() {
			messageText := ui.MessageInput.GetText()
			if messageText == "" {
				ui.NotificationBar.SetText("[red]Message cannot be empty")
				return
			}
			go ui.updateMessage(ui.SelectedTopicID, ui.CurrentMessageID, messageText)
		}).
		AddButton("Delete", func() {
			go ui.deleteMessage(ui.SelectedTopicID, ui.CurrentMessageID)
		})

	ui.MessageEditForm.SetBorder(true).SetTitle(" Edit Message ")

	ui.MessageEditForm.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			ui.Pages.SwitchToPage("messages-update-delete")
			ui.App.SetFocus(ui.MessagesUpdateDeleteView)
			return nil
		}
		return event
	})
}

// ////////////////////////////////////////////////////////ALL COMMANDS///////////////////////////////////////////////////////////////////////////////////////////////////////
func (ui *UI) topics() {
	ui.TopicsView.Clear()
	ui.TopicsView.AddItem("Loading topics...", "", 0, nil)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ui.Tail.ListTopics(ctx, &emptypb.Empty{}) //klici tail za podatke -- reads tail! (sprememba iz prej ka je bil c. ka je itak bil sam en client)
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.TopicsView.Clear()
			if status.Code(err) == codes.Unavailable {
				// če server ni dosegljiv
				// ponovno preveri head in tail
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel) // zamenjaj context, za test context.Background()
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.TopicsView.AddItem("Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.TopicsView.AddItem(fmt.Sprintf("Error: %v", err), "", 0, nil)
				return
			}
			if len(resp.Topics) == 0 {
				ui.TopicsView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			for _, t := range resp.Topics {
				topic := t
				ui.TopicsView.AddItem(
					fmt.Sprintf("[%d] %s", topic.Id, topic.Name),
					"",
					0,
					func() {
						ui.showMessages(topic.Id, topic.Name)
					},
				)
			}
		})
	}()
}

func (ui *UI) showMessages(topicID int64, name string) {
	ui.SelectedTopicID = topicID
	ui.MessagesView.Clear()
	ui.MessagesView.SetTitle(fmt.Sprintf("%d - %s", topicID, name))
	ui.Pages.SwitchToPage("messages")
	ui.App.SetFocus(ui.MessagesView)
	ui.MessagesView.AddItem("[yellow]Loading messages...", "", 0, nil)
	ui.Pages.SwitchToPage("messages")
	ui.App.SetFocus(ui.MessagesView)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		messages, err := ui.Tail.GetMessages(ctx, &pb.GetMessagesRequest{TopicId: topicID}) //list all messages always to tail
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.MessagesView.Clear()
			if status.Code(err) == codes.Unavailable {
				// če server ni dosegljiv
				// ponovno preveri head in tail
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel)
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.MessagesView.AddItem("[red]Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.MessagesView.AddItem(fmt.Sprintf("[red]Error: %v", err), "", 0, nil)
				return
			}
			if len(messages.Messages) == 0 {
				ui.MessagesView.AddItem("[gray]No messages in this topic yet.", "", 0, nil)
				return
			}
			for _, message := range messages.Messages {
				msg := message
				ui.MessagesView.AddItem(
					fmt.Sprintf("[%d] by %d", message.Id, message.UserId),
					fmt.Sprintf("%s (Likes: %d)", message.Text, message.Likes),
					0,
					func() {
						go ui.likeMessage(ui.SelectedTopicID, msg.Id)
					},
				)
			}
		})
	}()
}

func (ui *UI) showUpdateDeleteMessages(topicID int64, name string) {
	ui.SelectedTopicID = topicID
	ui.MessagesUpdateDeleteView.Clear()
	ui.MessagesUpdateDeleteView.SetTitle(fmt.Sprintf("%d - %s", topicID, name))
	ui.Pages.SwitchToPage("messages-update-delete")
	ui.App.SetFocus(ui.MessagesUpdateDeleteView)
	ui.MessagesUpdateDeleteView.AddItem("[yellow]Loading messages...", "", 0, nil)
	ui.Pages.SwitchToPage("messages-update-delete")
	ui.App.SetFocus(ui.MessagesUpdateDeleteView)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		messages, err := ui.Tail.GetMessages(ctx, &pb.GetMessagesRequest{TopicId: topicID}) //list all messages always to tail
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.MessagesUpdateDeleteView.Clear()
			if status.Code(err) == codes.Unavailable {
				// če server ni dosegljiv
				// ponovno preveri head in tail
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel)
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.MessagesUpdateDeleteView.AddItem("[red]Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.MessagesUpdateDeleteView.AddItem(fmt.Sprintf("[red]Error: %v", err), "", 0, nil)
				return
			}
			if len(messages.Messages) == 0 {
				ui.MessagesUpdateDeleteView.AddItem("[gray]No messages in this topic yet.", "", 0, nil)
				return
			}
			for _, message := range messages.Messages {
				msg := message
				ui.MessagesUpdateDeleteView.AddItem(
					fmt.Sprintf("[%d] by %d", message.Id, message.UserId),
					fmt.Sprintf("%s (Likes: %d)", message.Text, message.Likes),
					0,
					func() {
						ui.CurrentMessageID = msg.Id
						ui.MessageInput.SetText(msg.Text)
						ui.Pages.SwitchToPage("messageedit")
						ui.App.SetFocus(ui.MessageEditForm)
					},
				)
			}
		})
	}()
}

func (ui *UI) subscription() {
	ui.SubscriptionView.Clear()
	ui.SubscriptionView.AddItem("Loading topics...", "", 0, nil)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ui.Tail.ListTopics(ctx, &emptypb.Empty{}) //klici tail za podatke -- reads tail! (sprememba iz prej ka je bil c. ka je itak bil sam en client)
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.SubscriptionView.Clear()
			if status.Code(err) == codes.Unavailable {
				// če server ni dosegljiv
				// ponovno preveri head in tail
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel) // zamenjaj context, za test context.Background()
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.SubscriptionView.AddItem("Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.SubscriptionView.AddItem(fmt.Sprintf("Error: %v", err), "", 0, nil)
				return
			}
			if len(resp.Topics) == 0 {
				ui.SubscriptionView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			for _, t := range resp.Topics {
				topic := t
				ui.SubscriptionView.AddItem(
					fmt.Sprintf("[%d] %s", topic.Id, topic.Name),
					"",
					0,
					func() {
						go ui.subscribeLoop(ui.CP, topic.Id, ui.UserId)
					},
				)
			}
		})
	}()
}

func (ui *UI) newtopic(name string, input *tview.InputField) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to create topic: %v", err))
		} else {
			ui.NotificationBar.SetText(fmt.Sprintf("[green]Topic '%s' created!", name))
			input.SetText("") // Just clear the text, not the whole form
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		}
	})
}

func (ui *UI) loadSendMessageTopics() {
	ui.SendMessageTopicsView.Clear()
	ui.SendMessageTopicsView.AddItem("Loading topics...", "", 0, nil)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ui.Tail.ListTopics(ctx, &emptypb.Empty{})
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.SendMessageTopicsView.Clear()
			if status.Code(err) == codes.Unavailable {
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel)
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.SendMessageTopicsView.AddItem("Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.SendMessageTopicsView.AddItem(fmt.Sprintf("Error: %v", err), "", 0, nil)
				return
			}
			if len(resp.Topics) == 0 {
				ui.SendMessageTopicsView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			for _, t := range resp.Topics {
				topic := t
				ui.SendMessageTopicsView.AddItem(
					fmt.Sprintf("[%d] %s", topic.Id, topic.Name),
					"",
					0,
					func() {
						ui.SelectedTopicID = topic.Id
						ui.Pages.SwitchToPage("sendmessage")
						ui.App.SetFocus(ui.SendMessageForm)
					},
				)
			}
		})
	}()
}

func (ui *UI) sendMessage(messageText string, input *tview.InputField) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.PostMessage(ctx, &pb.PostMessageRequest{
		TopicId: ui.SelectedTopicID,
		Text:    messageText,
		UserId:  ui.UserId,
	})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to send message: %v", err))
		} else {
			ui.NotificationBar.SetText(fmt.Sprintf("[green]Message sent to topic %d!", ui.SelectedTopicID))
			input.SetText("")
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		}
	})
}

func (ui *UI) likeMessage(topicID int64, messageID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.LikeMessage(ctx, &pb.LikeMessageRequest{
		TopicId:   topicID,
		MessageId: messageID,
		UserId:    ui.UserId,
	})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to like message: %v", err))
		} else {
			ui.NotificationBar.SetText(fmt.Sprintf("[green]Liked message %d!", messageID))
			ui.showMessages(topicID, "")
		}
	})
}
func (ui *UI) updateDeleteTopics() {
	ui.UpdateDeleteView.Clear()
	ui.UpdateDeleteView.AddItem("Loading topics...", "", 0, nil)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ui.Tail.ListTopics(ctx, &emptypb.Empty{}) //klici tail za podatke -- reads tail! (sprememba iz prej ka je bil c. ka je itak bil sam en client)
		cancel()
		ui.App.QueueUpdateDraw(func() {
			ui.UpdateDeleteView.Clear()
			if status.Code(err) == codes.Unavailable {
				// če server ni dosegljiv
				// ponovno preveri head in tail
				headConn, tailConn := getHeadTailConn(ui.CP, context.Background(), cancel) // zamenjaj context, za test context.Background()
				ui.Head = pb.NewMessageBoardClient(headConn)
				ui.Tail = pb.NewMessageBoardClient(tailConn)
				ui.UpdateDeleteView.AddItem("Failed to load. Reconnected. Try again.", "", 0, nil)
				return
			}
			if err != nil {
				ui.UpdateDeleteView.AddItem(fmt.Sprintf("Error: %v", err), "", 0, nil)
				return
			}
			if len(resp.Topics) == 0 {
				ui.UpdateDeleteView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			for _, t := range resp.Topics {
				topic := t
				ui.UpdateDeleteView.AddItem(
					fmt.Sprintf("[%d] %s", topic.Id, topic.Name),
					"",
					0,
					func() {
						ui.showUpdateDeleteMessages(topic.Id, topic.Name)
					},
				)
			}
		})
	}()
}

func (ui *UI) updateMessage(topicID int64, messageID int64, messageText string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.UpdateMessage(ctx, &pb.UpdateMessageRequest{
		TopicId:   topicID,
		MessageId: messageID,
		Text:      messageText,
		UserId:    ui.UserId,
	})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to update message: %v", err))
		} else {
			ui.NotificationBar.SetText(fmt.Sprintf("[green]Message %d updated!", messageID))
			ui.MessageInput.SetText("")
			ui.showMessages(topicID, "")
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		}
	})
}

func (ui *UI) deleteMessage(topicID int64, messageID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.DeleteMessage(ctx, &pb.DeleteMessageRequest{
		TopicId:   topicID,
		MessageId: messageID,
		UserId:    ui.UserId,
	})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to delete message: %v", err))
		} else {
			ui.NotificationBar.SetText(fmt.Sprintf("[green]Message %d deleted!", messageID))
			ui.MessageInput.SetText("")
			ui.showMessages(topicID, "")
			ui.Pages.SwitchToPage("menu")
			ui.App.SetFocus(ui.Menu)
		}
	})
}
