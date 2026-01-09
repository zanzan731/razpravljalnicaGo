package client

import (
	"context"
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

	CP       pb.ControlPlaneClient
	CPConn   *grpc.ClientConn
	CPAddrs  *[]string
	Head     pb.MessageBoardClient
	Tail     pb.MessageBoardClient
	HeadConn *grpc.ClientConn
	TailConn *grpc.ClientConn

	// Cached list of all server addresses from control plane (chain)
	ServerAddrs []string
}

// reconnectHeadTail refreshes head/tail clients after a node failure.
func (ui *UI) reconnectHeadTail() {
	// Rediscover control plane leader first
	newCPAddr := getCPAddr(ui.CPAddrs)
	newCPConn, err := grpc.NewClient(newCPAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		//log.Printf("Failed to reconnect to control plane: %v", err)
		return
	}
	if ui.CPConn != nil {
		ui.CPConn.Close()
	}
	ui.CPConn = newCPConn
	ui.CP = pb.NewControlPlaneClient(newCPConn)

	// Now get fresh head/tail from updated control plane (non-fatal)
	headConn, tailConn, connErr := getHeadTailConn(ui.CP)
	if connErr != nil {
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText("[red]Failed to refresh head/tail endpoints; keeping current")
		})
		//log.Printf("Failed to refresh head/tail: %v", connErr)
		return
	}

	if ui.HeadConn != nil && ui.HeadConn != headConn {
		ui.HeadConn.Close()
	}
	if ui.TailConn != nil && ui.TailConn != tailConn && ui.TailConn != ui.HeadConn {
		ui.TailConn.Close()
	}

	ui.HeadConn = headConn
	ui.TailConn = tailConn
	ui.Head = pb.NewMessageBoardClient(headConn)
	ui.Tail = pb.NewMessageBoardClient(tailConn)

	// Refresh local cache of all server endpoints
	ui.refreshClusterCache()
	//log.Printf("Reconnected: head=%s tail=%s", headConn.Target(), tailConn.Target())
}

// refreshClusterCache fetches the full chain from control-plane and caches all node addresses
func (ui *UI) refreshClusterCache() {
	if ui.CP == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	state, err := ui.CP.GetClusterState(ctx, &emptypb.Empty{})
	cancel()
	if err != nil || state == nil {
		return
	}
	addrs := make([]string, 0, len(state.Chain))
	for _, cn := range state.Chain {
		if cn.GetInfo() != nil {
			addr := cn.GetInfo().GetAddress()
			if addr != "" {
				addrs = append(addrs, addr)
			}
		}
	}
	ui.ServerAddrs = addrs
}

// Subscribe samo ne dela sm si dal seznam vseh serverjov in pac naj prova skoz vse kdor mu odgovori mu pac odgovori ce noben unlucky
func (ui *UI) trySubscribeOnAnyNode(topicID int64, userID int64, token string, preferAddr string) (pb.MessageBoard_SubscribeTopicClient, *grpc.ClientConn, error) {
	// ce ni serverjev smo pac cooked
	if len(ui.ServerAddrs) == 0 {
		ui.refreshClusterCache()
	}
	candidates := make([]string, 0, len(ui.ServerAddrs)+1)
	if preferAddr != "" {
		candidates = append(candidates, preferAddr)
	}
	for _, a := range ui.ServerAddrs {
		if a == preferAddr {
			continue
		}
		candidates = append(candidates, a)
	}

	for _, addr := range candidates {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			continue
		}
		client := pb.NewMessageBoardClient(conn)
		stream, sErr := client.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
			TopicId:        []int64{topicID},
			UserId:         userID,
			SubscribeToken: token,
		})
		if sErr == nil {
			return stream, conn, nil
		}
		conn.Close()
	}
	return nil, nil, fmt.Errorf("no available node for subscription")
}

func getCPAddr(cpAddrs *[]string) string {
	for { // ponavlja dokler ne dobi leaderja
		for _, addr := range *cpAddrs {
			cpConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil { // poskusimo drugi naslov
				cpConn.Close()
				continue
			}
			cp := pb.NewControlPlaneClient(cpConn)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			res, err := cp.GetLeaderAddr(ctx, &emptypb.Empty{})
			cancel()
			if err == nil && res != nil && res.LeaderAddr != "" { // found leader
				return res.LeaderAddr
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func Run(cpAddrs *[]string) {
	// Dubi head in tail iz control plane
	var controlPlaneAddr string = getCPAddr(cpAddrs)
	//controlPlaneAddr = "localhost:" + controlPlaneAddr
	cpConn, err := grpc.NewClient(controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("control-plane connection failed:", err)
	}
	defer cpConn.Close() //da zpre conection predn se konca na koncu bi pozabu drgace, zdej a je defer slabsi ku ce napisem na koncu upam da to compiler pole prov nrdi ka jaz necem razmisljat o tem
	cp := pb.NewControlPlaneClient(cpConn)

	headConn, tailConn, connErr := getHeadTailConn(cp)
	if connErr != nil {
		log.Fatal("failed to get cluster state:", connErr)
	}
	headClient := pb.NewMessageBoardClient(headConn)
	tailClient := pb.NewMessageBoardClient(tailConn)

	app := tview.NewApplication()
	// zacetek logina
	var username string
	var password string
	var user *pb.User

	// kreiraj form
	usernameInput := tview.NewInputField().
		SetLabel("Username:").
		SetFieldWidth(20)

	passwordInput := tview.NewInputField().
		SetLabel("Password:").
		SetFieldWidth(20).
		SetMaskCharacter('*') //kr nice

	// displajcek mal ku un notification da ne ubijem ui in izpisujem pole na cmd sej umes pole ubijem ui za trenutek ma jebes se mi ne da spreminjat
	errorText := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	form := tview.NewForm().
		AddFormItem(usernameInput).
		AddFormItem(passwordInput).
		AddButton("Login", func() {
			errorText.SetText("")
			username = strings.TrimSpace(usernameInput.GetText()) //v usernamu ne zelim presledkov i dont care pac pole mi bojo dali sam presledke ni sans
			password = passwordInput.GetText()                    //tle bi tud lahk trimal sam pole bi lahk bil problem bi jim mogu vsaj rect

			if username == "" || password == "" {
				errorText.SetText("[red]Username and password cannot be empty")
				return
			}
			if len(password) < 3 || len(password) >= 20 {
				errorText.SetText("[red]Password must be at least 3 characters and less than 20 characters long")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			user, err = headClient.LoginUser(ctx, &pb.LoginRequest{
				Username: username,
				Password: password,
			})
			cancel()
			if err != nil {
				//ben mu dajamo ful podatkov profesor za spletno se bi ze jokal ku je to slabo ma pac jebes
				errorText.SetText(fmt.Sprintf("[red]Login failed: %v", err))
				return
			}
			//tle mal breakam app za sekundo ce se kej zalomi in tko unlucky
			app.Stop()
		}).
		AddButton("Register", func() {
			errorText.SetText("")
			username = strings.TrimSpace(usernameInput.GetText())
			password = passwordInput.GetText()

			if username == "" || password == "" {
				errorText.SetText("[red]Username and password cannot be empty")
				return
			}
			if len(password) < 3 || len(password) >= 20 {
				errorText.SetText("[red]Password must be at least 3 characters and less than 20 characters long")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			user, err = headClient.RegisterUser(ctx, &pb.RegisterRequest{
				Username: username,
				Password: password,
			})
			cancel()
			if err != nil {
				errorText.SetText(fmt.Sprintf("[red]Registration failed: %v", err))
				return
			}
			app.Stop()
		})

	// Error na dnu in form zgori
	loginLayout := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(form, 0, 1, true).
		AddItem(errorText, 1, 0, false)
	//ta EnableMouse bi mogu tud spodi iskreno
	if err := app.SetRoot(loginLayout, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}

	if user == nil {
		headConn.Close()
		if tailConn != nil && tailConn != headConn {
			tailConn.Close()
		}
		log.Fatal("failed to login or register")
	}
	fmt.Println("Logged in as user: ", user.Name, "ID: ", user.Id)

	//Interactive Shell -- po novem rabi vse podatke o tem komu posiljat kaj... treba cekirat tud na serverju vse da se na pravih krajih bere in pise
	runShell(cp, cpConn, cpAddrs, headClient, headConn, tailClient, tailConn, user.Id)
	//zpri se head in tail connection
	headConn.Close()
	//spomni se od prej lahk sta ista ne mores zprt ze zaprte povezave
	if tailConn != nil && tailConn != headConn {
		tailConn.Close()
	}
}

// UI reconection mal je pomagu catko ka moj je sam crashnu clienta ne bomo niti idk zakaj
func getHeadTailConn(cp pb.ControlPlaneClient) (*grpc.ClientConn, *grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state, err := cp.GetClusterState(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, nil, err
	}
	headAddr := state.GetHead().GetAddress()
	tailAddr := state.GetTail().GetAddress()

	headConn, err := grpc.NewClient(headAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("head connection failed: %w", err)
	}
	var tailConn *grpc.ClientConn
	if tailAddr == headAddr {
		tailConn = headConn
	} else {
		tailConn, err = grpc.NewClient(tailAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			headConn.Close()
			return nil, nil, fmt.Errorf("tail connection failed: %w", err)
		}
	}
	return headConn, tailConn, nil
}

// this function has 8 parameters which is more than 7 advised ---who cares sam pusti me namiru dobesedno je to centralni del kode dej mir
func runShell(cp pb.ControlPlaneClient, cpConn *grpc.ClientConn, cpAddrs *[]string, head pb.MessageBoardClient, headConn *grpc.ClientConn, tail pb.MessageBoardClient, tailConn *grpc.ClientConn, userID int64) {
	app := tview.NewApplication()
	_ = NewUI(app, cp, cpConn, cpAddrs, head, headConn, tail, tailConn, userID)
	//dodal EnableMouse mislim da rabim sam tle upam
	if err := app.EnableMouse(true).Run(); err != nil {
		panic(err)
	}
}

func (ui *UI) subscribeLoop(topicID int64, userID int64) {
	ui.SubscriptionView.Clear()
	ui.App.QueueUpdateDraw(func() {
		ui.SubscriptionView.AddItem(fmt.Sprintf("[yellow]Subscribing to topic %d...\n", topicID), "", 0, nil)
	})

	// prvo cekiri control plane
	if ui.CP == nil || ui.CPConn == nil {
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText("[yellow]Control-plane unavailable, rediscovering...")
		})
		ui.reconnectHeadTail()
		if ui.CP == nil || ui.CPConn == nil {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[red]Failed to reconnect to control-plane")
			})
			return
		}
	}

	// provi sub node cene pac
	var resp *pb.SubscriptionNodeResponse
	var err error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err = ui.CP.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: userID, TopicId: []int64{topicID}})
		cancel()
		if err == nil {
			break
		}
		if status.Code(err) == codes.Unavailable {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[yellow]Control-plane unavailable, rediscovering...")
			})
			ui.reconnectHeadTail()
			if ui.CP == nil || ui.CPConn == nil {
				ui.App.QueueUpdateDraw(func() {
					ui.NotificationBar.SetText("[red]Failed to reconnect to control-plane")
				})
				return
			}
		} else {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[red]Subscription node lookup failed")
			})
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	if resp == nil || resp.GetNode() == nil {
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText("[red]Subscription node response invalid")
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
			ui.NotificationBar.SetText("[red]Connect to subscription node failed")
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
		// First, try any cached node with the current token
		subConn.Close()
		streamAny, connAny, anyErr := ui.trySubscribeOnAnyNode(topicID, userID, resp.GetSubscribeToken(), addr)
		if anyErr == nil {
			stream = streamAny
			subConn = connAny
		} else {
			// If that fails, reconnect control-plane, re-fetch token, then try any node again
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[yellow]Subscription node unavailable; refreshing cluster and token...")
			})
			ui.reconnectHeadTail()
			// Re-fetch token
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			newResp, err2 := ui.CP.GetSubscriptionNode(ctx, &pb.SubscriptionNodeRequest{UserId: userID, TopicId: []int64{topicID}})
			cancel()
			if err2 != nil || newResp == nil {
				ui.App.QueueUpdateDraw(func() {
					ui.NotificationBar.SetText("[red]Failed to get new subscription token")
				})
				return
			}
			resp = newResp
			streamAny2, connAny2, anyErr2 := ui.trySubscribeOnAnyNode(topicID, userID, resp.GetSubscribeToken(), resp.GetNode().GetAddress())
			if anyErr2 != nil {
				ui.App.QueueUpdateDraw(func() {
					ui.NotificationBar.SetText("[red]Failed to subscribe after scanning all nodes")
				})
				return
			}
			stream = streamAny2
			subConn = connAny2
		}
	}

	ui.App.QueueUpdateDraw(func() {
		ui.SubscriptionView.AddItem(fmt.Sprintf("[green]Subscribed to topic %d", topicID), "", 0, nil)
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Subscribed to topic %d - Waiting for new messages...", topicID))
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})

	// Listen for messages; if stream fails, attempt to reconnect
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText(fmt.Sprintf("[yellow]Stream closed for topic %d", topicID))
			})
			return
		}
		if err != nil {
			// Stream error: try reconnecting and resubscribing
			if status.Code(err) == codes.Unavailable {
				ui.App.QueueUpdateDraw(func() {
					ui.NotificationBar.SetText("[yellow]Subscription node lost, reconnecting...")
				})
				subConn.Close()
				// Recursive call to retry subscription
				go ui.subscribeLoop(topicID, userID)
				return
			}
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[red]Stream error - node disconnected, retrying subscription...")
			})
			subConn.Close()
			go ui.subscribeLoop(topicID, userID)
			return
		}
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText(fmt.Sprintf("[cyan]NEW MSG in topic %d: %s", topicID, event.Message.Text))
		})
	}
}

// ////////////////////////////////////////////////////////////////// UI ///////////////////////////////////////////////////////////
// This function has 9 parameters, which is greater than the 7 authorized. ---ne bom vec komentiral
func NewUI(app *tview.Application, cp pb.ControlPlaneClient, cpConn *grpc.ClientConn, cpAddrs *[]string, head pb.MessageBoardClient, headConn *grpc.ClientConn, tail pb.MessageBoardClient, tailConn *grpc.ClientConn, userId int64) *UI {
	ui := &UI{
		App:      app,
		Pages:    tview.NewPages(),
		CP:       cp,
		CPConn:   cpConn,
		CPAddrs:  cpAddrs,
		Head:     head,
		HeadConn: headConn,
		Tail:     tail,
		TailConn: tailConn,
		UserId:   userId,
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
		AddItem(ui.Pages, 0, 3, true).
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
				ui.NotificationBar.SetText("[red]You can't put an empty string for topic name")
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
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText("[red]Error loading topics")
				return
			}
			if len(resp.Topics) == 0 {
				ui.TopicsView.Clear()
				ui.TopicsView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			ui.TopicsView.Clear()
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
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText("[red]Error loading messages")
				return
			}
			ui.MessagesView.Clear()
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
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText(fmt.Sprintf("[red]Error loading messages: %v", err))
				return
			}
			ui.MessagesUpdateDeleteView.Clear()
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
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText("[red]Error loading topics")
				return
			}
			if len(resp.Topics) == 0 {
				ui.SubscriptionView.Clear()
				ui.SubscriptionView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			ui.SubscriptionView.Clear()
			for _, t := range resp.Topics {
				topic := t
				ui.SubscriptionView.AddItem(
					fmt.Sprintf("[%d] %s", topic.Id, topic.Name),
					"",
					0,
					func() {
						go ui.subscribeLoop(topic.Id, ui.UserId)
					},
				)
			}
		})
	}()
}

func (ui *UI) newtopic(name string, input *tview.InputField) {
	// Guard against missing head connection/client
	if ui.HeadConn == nil || ui.Head == nil {
		ui.NotificationBar.SetText("[red]Head unavailable; try again after reconnect")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name})
	cancel()

	ui.App.QueueUpdateDraw(func() {
		if status.Code(err) == codes.Unavailable {
			ui.NotificationBar.SetText("[yellow]Reconnecting to new head...")
			ui.reconnectHeadTail()
			// After reconnect, ensure head is ready before retrying
			if ui.HeadConn == nil || ui.Head == nil {
				ui.NotificationBar.SetText("[red]Head still unavailable after reconnect")
				return
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = ui.Head.CreateTopic(ctx2, &pb.CreateTopicRequest{Name: name})
			cancel2()
		}
		if err != nil {
			ui.NotificationBar.SetText("[red]Failed to create topic - head unavailable")
			return
		}
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Topic '%s' created!", name)) //buljs za njih da ne bojo ta imena 100 znakov ka sm vse drugo zbrisal za probleme za UI tko da je sam se tle
		input.SetText("")
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
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
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText("[red]Error loading topics")
				return
			}
			if len(resp.Topics) == 0 {
				ui.SendMessageTopicsView.AddItem("There are no open topics yet!", "", 0, nil)
				return
			}
			ui.SendMessageTopicsView.Clear()
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
	// Prvo provi ce je topic izbran
	if ui.SelectedTopicID == 0 {
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText("[red]Select a topic first")
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ui.Head.PostMessage(ctx, &pb.PostMessageRequest{
		TopicId: ui.SelectedTopicID,
		Text:    messageText,
		UserId:  ui.UserId,
	})
	cancel()

	// If unavailable, try to reconnect and retry (outside UI thread)
	if status.Code(err) == codes.Unavailable {
		ui.App.QueueUpdateDraw(func() {
			ui.NotificationBar.SetText("[yellow]Reconnecting to new head...")
		})
		ui.reconnectHeadTail()
		// Ensure head exists after reconnect
		if ui.Head == nil || ui.HeadConn == nil {
			ui.App.QueueUpdateDraw(func() {
				ui.NotificationBar.SetText("[red]Head unavailable after reconnect; try again")
			})
			return
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		_, err = ui.Head.PostMessage(ctx2, &pb.PostMessageRequest{
			TopicId: ui.SelectedTopicID,
			Text:    messageText,
			UserId:  ui.UserId,
		})
		cancel2()
	}

	// UI updates based on final outcome
	ui.App.QueueUpdateDraw(func() {
		if err != nil {
			switch status.Code(err) {
			case codes.Unavailable:
				ui.NotificationBar.SetText("[red]Failed to send: head unavailable")
			case codes.NotFound:
				ui.NotificationBar.SetText("[red]Topic not found")
			case codes.InvalidArgument:
				ui.NotificationBar.SetText("[red]Invalid message or topic")
			default:
				ui.NotificationBar.SetText(fmt.Sprintf("[red]Failed to send message: %v", err))
			}
			return
		}
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Message sent to topic %d!", ui.SelectedTopicID))
		input.SetText("")
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
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
		if status.Code(err) == codes.Unavailable {
			ui.NotificationBar.SetText("[yellow]Reconnecting to new head...")
			ui.reconnectHeadTail()
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = ui.Head.LikeMessage(ctx2, &pb.LikeMessageRequest{
				TopicId:   topicID,
				MessageId: messageID,
				UserId:    ui.UserId,
			})
			cancel2()
		}
		if err != nil {
			//ist ku prej za delete in update
			if strings.Contains(err.Error(), "you can't like your own message") {
				ui.NotificationBar.SetText("[red]You can't like your own messages")
				return
			}
			if strings.Contains(err.Error(), "you already liked this message") {
				ui.NotificationBar.SetText("[red]You already liked this message, you can only like a message once")
				return
			}
			ui.NotificationBar.SetText("[red]Failed to like message - head unavailable")
			return
		}
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Liked message %d!", messageID))
		ui.showMessages(topicID, "")
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})
}
func (ui *UI) updateDeleteTopics() {
	ui.UpdateDeleteView.Clear()
	ui.UpdateDeleteView.AddItem("Loading topics...", "", 0, nil)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ui.Tail.ListTopics(ctx, &emptypb.Empty{}) //klici tail za podatke -- reads tail! (sprememba iz prej ka je bil c. ka je itak bil sam en client)
		cancel()
		ui.UpdateDeleteView.Clear()
		ui.App.QueueUpdateDraw(func() {
			if status.Code(err) == codes.Unavailable {
				ui.reconnectHeadTail()
				if ui.Head == nil || ui.Tail == nil {
					ui.NotificationBar.SetText("[red]Failed to refresh endpoints; please try again")
					return
				}
				ui.NotificationBar.SetText("[green]Reconnected. Try again.")
				return
			}
			if err != nil {
				ui.NotificationBar.SetText(fmt.Sprintf("[red]Error loading topics: %v", err))
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
		if status.Code(err) == codes.Unavailable {
			ui.NotificationBar.SetText("[yellow]Reconnecting to new head...")
			ui.reconnectHeadTail()
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = ui.Head.UpdateMessage(ctx2, &pb.UpdateMessageRequest{
				TopicId:   topicID,
				MessageId: messageID,
				Text:      messageText,
				UserId:    ui.UserId,
			})
			cancel2()
		}
		if err != nil {
			//zato da ves da updatas napacn message ki ni tvoj
			if strings.Contains(err.Error(), "user is not the owner") {
				ui.NotificationBar.SetText("[red]You can only edit your own messages")
				return
			}
			ui.NotificationBar.SetText("[red]Failed to update message")
			return
		}
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Message %d updated!", messageID))
		ui.MessageInput.SetText("")
		ui.showMessages(topicID, "")
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
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
		if status.Code(err) == codes.Unavailable {
			ui.NotificationBar.SetText("[yellow]Reconnecting to new head...")
			ui.reconnectHeadTail()
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = ui.Head.DeleteMessage(ctx2, &pb.DeleteMessageRequest{
				TopicId:   topicID,
				MessageId: messageID,
				UserId:    ui.UserId,
			})
			cancel2()
		}
		if err != nil {
			if strings.Contains(err.Error(), "user is not the owner") {
				ui.NotificationBar.SetText("[red]You can only delete your own messages")
				return
			}
			ui.NotificationBar.SetText("[red]Failed to delete message")
			return
		}
		ui.NotificationBar.SetText(fmt.Sprintf("[green]Message %d deleted!", messageID))
		ui.MessageInput.SetText("")
		ui.showMessages(topicID, "")
		ui.Pages.SwitchToPage("menu")
		ui.App.SetFocus(ui.Menu)
	})
}
