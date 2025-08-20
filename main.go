package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	_ "modernc.org/sqlite"

	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- Types ---
type OutgoingWebhookPayload struct {
	Sender  string `json:"sender"`
	Message string `json:"message"`
}

type IncomingSendRequest struct {
	Number string `json:"number"`
	Text   string `json:"text"`
}

// --- Helpers ---
func sendToWebhook(endpoint, endpointTest, user, pass string, payload OutgoingWebhookPayload) {
	fmt.Println(payload.Sender, payload.Message)
	domains := [2]string{endpoint, endpointTest}
	for _, domain := range domains {
		data, _ := json.Marshal(payload)
		req, _ := http.NewRequest("POST", domain, bytes.NewBuffer(data))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(user, pass)
		client := &http.Client{}
		res, err := client.Do(req)
		if err != nil {
			fmt.Printf("----->>> Error in calling webhook, %T\n", err)
		} else if res.StatusCode != 200 {
			fmt.Printf("----->>> Error in calling webhook, %s\n", res.Status)
		} else {
			break
		}
	}
}

// --- Config & Client Setup ---
func loadConfig() (webhook, webhookTest, user, pass, listenAddr string) {
	_ = godotenv.Load()
	webhook = os.Getenv("WEBHOOK_URL")
	webhookTest = os.Getenv("WEBHOOK_URL_TEST")
	user = os.Getenv("WEBHOOK_USER")
	pass = os.Getenv("WEBHOOK_PASS")
	listenAddr = os.Getenv("LISTEN_ADDR")
	if webhook == "" || user == "" || pass == "" || listenAddr == "" {
		panic("WEBHOOK_URL, WEBHOOK_USER, WEBHOOK_PASS, LISTEN_ADDR must be set in .env")
	}
	return
}

func newClient(ctx context.Context) *whatsmeow.Client {
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	container, err := sqlstore.New(ctx, "sqlite", "file:session.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbLog)
	if err != nil {
		panic(err)
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		panic(err)
	}
	clientLog := waLog.Stdout("Client", "INFO", true)
	return whatsmeow.NewClient(deviceStore, clientLog)
}

// --- Handlers ---
func registerMessageHandler(client *whatsmeow.Client, webhook, webhookTest, user, pass string) {
	client.AddEventHandler(func(evt interface{}) {
		if v, ok := evt.(*events.Message); ok && !v.Info.MessageSource.IsFromMe {
			var text string

			if v.Message.GetConversation() != "" {
				text = v.Message.GetConversation()
			} else if v.Message.ExtendedTextMessage != nil {
				text = v.Message.ExtendedTextMessage.GetText()
			} else if v.Message.ImageMessage != nil {
				text = v.Message.ImageMessage.GetCaption()
			} else if v.Message.VideoMessage != nil {
				text = v.Message.VideoMessage.GetCaption()
			}

			// Send text
			if text != "" {
				go sendToWebhook(webhook, webhookTest, user, pass, OutgoingWebhookPayload{
					Sender:  v.Info.Sender.User,
					Message: text,
				})
			}

			// Handle voice notes
			// if v.Message.AudioMessage != nil {
			// 	data, err := client.Download(context.Background(), v.Message.AudioMessage)
			// 	if err != nil {
			// 		fmt.Println("failed to download audio:", err)
			// 		return
			// 	}
			// 	fmt.Println("voice received", len(data))
			// }
		}
	})
}

func startHTTP(ctx context.Context, client *whatsmeow.Client, listenAddr string) {
	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "only POST allowed", http.StatusMethodNotAllowed)
			return
		}

		var req IncomingSendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Number == "" || req.Text == "" {
			http.Error(w, "number and text required", http.StatusBadRequest)
			return
		}

		jid := types.NewJID(req.Number, "s.whatsapp.net")
		msg := &waProto.Message{
			Conversation: proto.String(req.Text),
		}

		if _, err := client.SendMessage(ctx, jid, msg); err != nil {
			http.Error(w, "failed to send", http.StatusInternalServerError)
			return
		}

		w.Write([]byte("sent"))
	})

	go func() {
		fmt.Println("HTTP server on", listenAddr)
		if err := http.ListenAndServe(listenAddr, nil); err != nil {
			panic(err)
		}
	}()
}

// --- WhatsApp Client ---
func startClient(ctx context.Context, client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("QR code:", evt.Code)
			}
		}
	} else {
		if err := client.Connect(); err != nil {
			panic(err)
		}
	}
}

// --- Main ---
func main() {
	ctx := context.Background()
	webhook, webhookTest, user, pass, listenAddr := loadConfig()
	client := newClient(ctx)

	registerMessageHandler(client, webhook, webhookTest, user, pass)
	startHTTP(ctx, client, listenAddr)
	startClient(ctx, client)

	fmt.Println("Bot is running. Press Ctrl+C to quit.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	client.Disconnect()
}
