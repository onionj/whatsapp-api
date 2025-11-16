package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	_ "modernc.org/sqlite"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type OutgoingWebhookPayload struct {
	Sender  string `json:"sender"`
	Message string `json:"message"`
	Voice   []byte `json:"voice,omitempty"`
	IsGroup bool   `json:"is_group"`
}

type IncomingSendRequest struct {
	Number  string `json:"number"`
	Text    string `json:"text"`
	IsGroup bool   `json:"is_group"`
}

var (
	textWebhook  string
	voiceWebhook string
	user         string
	pass         string
	listenAddr   string
	version      string
)

func init() {
	_ = godotenv.Load()
	textWebhook = os.Getenv("TEXT_WEBHOOK_URL")
	voiceWebhook = os.Getenv("VOICE_WEBHOOK_URL")
	user = os.Getenv("WEBHOOK_USER")
	pass = os.Getenv("WEBHOOK_PASS")
	listenAddr = os.Getenv("LISTEN_ADDR")
	if voiceWebhook == "" || textWebhook == "" || user == "" || pass == "" || listenAddr == "" {
		panic("TEXT_WEBHOOK_URL, VOICE_WEBHOOK_URL, WEBHOOK_USER, WEBHOOK_PASS, LISTEN_ADDR must be set in .env")
	}
}

// sendToWebhook is a helper function to send new whatsapp message to the configured webhook
func sendToWebhook(payload OutgoingWebhookPayload) {
	fmt.Println(payload.Sender, payload.Message, "IsGroup:", payload.IsGroup)
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", textWebhook, bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error in calling webhook, %T\n", err)
	} else if res.StatusCode != 200 {
		fmt.Printf("Error in calling webhook, %s\n", res.Status)
	}
}

// sendToWebhookVoice is a helper function to send new whatsapp voice note to the configured webhook
func sendToWebhookVoice(payload OutgoingWebhookPayload) {
	fmt.Println(payload.Sender, "voice", len(payload.Voice), "IsGroup:", payload.IsGroup)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// include sender as a field
	_ = writer.WriteField("sender", payload.Sender)
	_ = writer.WriteField("is_group", strconv.FormatBool(payload.IsGroup))

	// choose filename & content-type based on header detection
	filename := "file.oga"
	contentType := "audio/ogg; codecs=opus"
	if !(len(payload.Voice) >= 4 && string(payload.Voice[:4]) == "OggS") {
		// fallback if the file isn't an OGG container
		filename = "file.opus"
		contentType = "audio/opus"
	}

	partHeaders := textproto.MIMEHeader{}
	partHeaders.Set("Content-Disposition", fmt.Sprintf(`form-data; name="data"; filename="%s"`, filename))
	partHeaders.Set("Content-Type", contentType)

	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		fmt.Println("failed to create multipart part:", err)
		_ = writer.Close()
		return
	}

	// write payload to the part
	if _, err := io.Copy(part, bytes.NewReader(payload.Voice)); err != nil {
		fmt.Println("failed to write payload to multipart part:", err)
		_ = writer.Close()
		return
	}

	if err := writer.Close(); err != nil {
		fmt.Println("failed to close multipart writer:", err)
		return
	}

	req, err := http.NewRequest("POST", voiceWebhook, body)
	if err != nil {
		fmt.Println("failed to build request:", err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType()) // includes boundary
	req.SetBasicAuth(user, pass)

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error in calling webhook, %T\n", err)
		return
	}
	if res.StatusCode != 200 {
		fmt.Printf("Error in calling webhook, %s\n", res.Status)
		return
	}
}

// newClient initializes a new WhatsApp client using whatsmeow and a SQLite database
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

// registerMessageHandler sets up a handler to process incoming whatsapp messages
func registerMessageHandler(client *whatsmeow.Client) {
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

			var sender string

			switch v.Info.Chat.Server {
			case types.HiddenUserServer:
				sender = v.Info.MessageSource.SenderAlt.User
			case types.GroupServer:
				sender = v.Info.MessageSource.Chat.User
			default:
				sender = v.Info.Sender.User
			}

			// Send text
			if text != "" {
				go sendToWebhook(OutgoingWebhookPayload{
					Sender:  sender,
					Message: text,
					IsGroup: v.Info.IsGroup,
				})
				return
			}
			// Handle voice notes
			if v.Message.AudioMessage != nil {
				data, err := client.Download(context.Background(), v.Message.AudioMessage)
				if err != nil {
					fmt.Println("failed to download audio:", err)
					return
				}
				go sendToWebhookVoice(OutgoingWebhookPayload{
					Sender:  sender,
					Voice:   data,
					IsGroup: v.Info.IsGroup,
				})
			}
		}
	})
}

// logging http log middleware
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		fmt.Printf("%s %s %s\n", r.Method, r.URL.Path, time.Since(start))
	})
}

// startHTTP starts an HTTP server to handle incoming requests to send WhatsApp messages
func startHTTP(ctx context.Context, client *whatsmeow.Client, listenAddr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "only POST allowed", http.StatusMethodNotAllowed)
			return
		}
		// Basic Auth
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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

		var jid types.JID
		if req.IsGroup {
			jid = types.NewJID(req.Number, types.GroupServer)
		} else {
			jid = types.NewJID(req.Number, types.DefaultUserServer)
		}
		msg := &waProto.Message{
			Conversation: proto.String(req.Text),
		}

		if _, err := client.SendMessage(ctx, jid, msg); err != nil {
			http.Error(w, "failed to send: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Write([]byte("sent"))
	})

	go func() {
		fmt.Println("HTTP server on", listenAddr)
		if err := http.ListenAndServe(listenAddr, logging(mux)); err != nil {
			panic(err)
		}
	}()
}

// startClient connects the WhatsApp client, handling QR code generation if needed
func startClient(ctx context.Context, client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("Scan the QR code below to connect to WhatsApp:")
				qrterminal.Generate(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		if err := client.Connect(); err != nil {
			panic(err)
		}
	}
}

func main() {
	fmt.Println("version " + version)
	ctx := context.Background()
	client := newClient(ctx)

	registerMessageHandler(client)
	startHTTP(ctx, client, listenAddr)
	startClient(ctx, client)

	fmt.Println("Bot is running. Press Ctrl+C to quit.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	client.Disconnect()
}
