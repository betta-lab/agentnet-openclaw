package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/betta-lab/agentnet-openclaw/internal/client"
	"github.com/betta-lab/agentnet-openclaw/internal/keystore"
)

// Daemon manages an AgentNet connection and exposes a local HTTP API.
type Daemon struct {
	addr      string
	relay     string
	agentName string
	keyPath   string
	client    *client.Client
	mu        sync.RWMutex
	messages  []client.IncomingMessage // ring buffer
}

// Config holds daemon configuration.
type Config struct {
	ListenAddr string // e.g. "127.0.0.1:9900"
	RelayURL   string // e.g. "wss://relay.example.com/v1/ws"
	AgentName  string
	DataDir    string // for key storage
}

// New creates a daemon (does not start it).
func New(cfg Config) *Daemon {
	keyPath := filepath.Join(cfg.DataDir, "agent.key")
	return &Daemon{
		addr:      cfg.ListenAddr,
		relay:     cfg.RelayURL,
		agentName: cfg.AgentName,
		keyPath:   keyPath,
		messages:  make([]client.IncomingMessage, 0, 1000),
	}
}

// Start connects to the relay and starts the HTTP API.
func (d *Daemon) Start() error {
	keys, err := keystore.LoadOrCreate(d.keyPath)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}

	log.Printf("agent ID: %s", keys.AgentID())
	log.Printf("connecting to relay: %s", d.relay)

	c, err := client.Connect(d.relay, keys.AgentID(), d.agentName, keys.PrivateKey)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	d.mu.Lock()
	d.client = c
	d.mu.Unlock()

	// Collect incoming messages
	go d.collectMessages()

	// Write PID file
	pidPath := filepath.Join(filepath.Dir(d.keyPath), "daemon.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)

	mux := http.NewServeMux()
	mux.HandleFunc("/status", d.handleStatus)
	mux.HandleFunc("/rooms", d.handleRooms)
	mux.HandleFunc("/rooms/create", d.handleCreateRoom)
	mux.HandleFunc("/rooms/join", d.handleJoinRoom)
	mux.HandleFunc("/rooms/leave", d.handleLeaveRoom)
	mux.HandleFunc("/send", d.handleSend)
	mux.HandleFunc("/messages", d.handleMessages)
	mux.HandleFunc("/stop", d.handleStop)

	log.Printf("HTTP API on %s", d.addr)
	return http.ListenAndServe(d.addr, mux)
}

func (d *Daemon) collectMessages() {
	for msg := range d.client.Messages() {
		d.mu.Lock()
		if len(d.messages) >= 1000 {
			d.messages = d.messages[1:]
		}
		d.messages = append(d.messages, msg)
		d.mu.Unlock()
	}
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	connected := d.client != nil
	d.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":  connected,
		"relay":      d.relay,
		"agent_name": d.agentName,
	})
}

func (d *Daemon) handleRooms(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	c := d.client
	d.mu.RUnlock()
	if c == nil {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}

	rooms, err := c.ListRooms(nil, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(rooms)
}

func (d *Daemon) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Room  string   `json:"room"`
		Topic string   `json:"topic"`
		Tags  []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	d.mu.RLock()
	c := d.client
	d.mu.RUnlock()
	if c == nil {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}

	info, err := c.CreateRoom(req.Room, req.Topic, req.Tags)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(info)
}

func (d *Daemon) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Room string `json:"room"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	d.mu.RLock()
	c := d.client
	d.mu.RUnlock()
	if c == nil {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}

	info, err := c.JoinRoom(req.Room)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(info)
}

func (d *Daemon) handleLeaveRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Room string `json:"room"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	d.mu.RLock()
	c := d.client
	d.mu.RUnlock()
	if c == nil {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}

	if err := c.LeaveRoom(req.Room); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (d *Daemon) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Room string `json:"room"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	d.mu.RLock()
	c := d.client
	d.mu.RUnlock()
	if c == nil {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}

	if err := c.SendMessage(req.Room, req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (d *Daemon) handleMessages(w http.ResponseWriter, r *http.Request) {
	roomFilter := r.URL.Query().Get("room")

	d.mu.RLock()
	var msgs []client.IncomingMessage
	for _, m := range d.messages {
		if roomFilter == "" || strings.EqualFold(m.Room, roomFilter) {
			msgs = append(msgs, m)
		}
	}
	d.mu.RUnlock()

	// Return last 50
	if len(msgs) > 50 {
		msgs = msgs[len(msgs)-50:]
	}

	json.NewEncoder(w).Encode(msgs)
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
	go func() {
		d.mu.Lock()
		if d.client != nil {
			d.client.Close()
		}
		d.mu.Unlock()
		os.Exit(0)
	}()
}
