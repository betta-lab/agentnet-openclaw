package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/betta-lab/agentnet-openclaw/internal/client"
	"github.com/betta-lab/agentnet-openclaw/internal/keystore"
)

// Daemon manages an AgentNet connection and exposes a local HTTP API.
type Daemon struct {
	addr           string
	relay          string
	agentName      string
	keyPath        string
	apiToken       string
	client         *client.Client
	mu             sync.RWMutex
	messages       []client.IncomingMessage // ring buffer
	joinedRooms    map[string]bool          // rooms to rejoin on reconnect
	keys           *keystore.Keys
	version        string
	latestVersion  string // fetched async on startup
}

// Config holds daemon configuration.
type Config struct {
	ListenAddr string // e.g. "127.0.0.1:9900"
	RelayURL   string // e.g. "wss://relay.example.com/v1/ws"
	AgentName  string
	DataDir    string // for key storage
	Version    string // current binary version
}

// New creates a daemon (does not start it).
func New(cfg Config) *Daemon {
	keyPath := filepath.Join(cfg.DataDir, "agent.key")
	return &Daemon{
		addr:        cfg.ListenAddr,
		relay:       cfg.RelayURL,
		agentName:   cfg.AgentName,
		keyPath:     keyPath,
		messages:    make([]client.IncomingMessage, 0, 1000),
		joinedRooms: make(map[string]bool),
		version:     cfg.Version,
	}
}

// Start connects to the relay and starts the HTTP API.
func (d *Daemon) Start() error {
	// Generate API token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	d.apiToken = hex.EncodeToString(tokenBytes)

	// Write token file
	tokenPath := filepath.Join(filepath.Dir(d.keyPath), "api.token")
	if err := os.WriteFile(tokenPath, []byte(d.apiToken), 0600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	log.Printf("API token written to %s", tokenPath)

	keys, err := keystore.LoadOrCreate(d.keyPath)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}

	// Default name: "agent-<first8chars of ID>" — never use hostname (leaks server identity)
	if d.agentName == "" {
		id := keys.AgentID()
		if len(id) > 8 {
			id = id[:8]
		}
		d.agentName = "agent-" + id
	}

	log.Printf("agent ID: %s", keys.AgentID())
	log.Printf("agent name: %s", d.agentName)
	log.Printf("connecting to relay: %s", d.relay)

	d.keys = keys

	// Initial connect
	if err := d.connectAndRejoin(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	// Reconnect loop — watches for disconnection and reconnects with backoff
	go d.reconnectLoop()

	// Check for updates periodically (non-blocking)
	go d.versionCheckLoop()

	// Write PID file
	pidPath := filepath.Join(filepath.Dir(d.keyPath), "daemon.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0600)

	mux := http.NewServeMux()
	mux.HandleFunc("/status", d.requireAuth(d.handleStatus))
	mux.HandleFunc("/rooms", d.requireAuth(d.handleRooms))
	mux.HandleFunc("/rooms/create", d.requireAuth(d.handleCreateRoom))
	mux.HandleFunc("/rooms/join", d.requireAuth(d.handleJoinRoom))
	mux.HandleFunc("/rooms/leave", d.requireAuth(d.handleLeaveRoom))
	mux.HandleFunc("/send", d.requireAuth(d.handleSend))
	mux.HandleFunc("/messages", d.requireAuth(d.handleMessages))
	mux.HandleFunc("/history", d.requireAuth(d.handleHistory))
	mux.HandleFunc("/stop", d.requireAuth(d.handleStop))

	log.Printf("HTTP API on %s", d.addr)
	return http.ListenAndServe(d.addr, mux)
}

func (d *Daemon) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+d.apiToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// versionCheckLoop checks for updates on startup and every 6 hours.
func (d *Daemon) versionCheckLoop() {
	d.checkLatestVersion()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		d.checkLatestVersion()
	}
}

// checkLatestVersion fetches the latest release from GitHub and caches it.
func (d *Daemon) checkLatestVersion() {
	c := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/betta-lab/agentnet-openclaw/releases/latest", nil)
	req.Header.Set("User-Agent", "agentnet-daemon/"+d.version)
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(func() []byte { b, _ := io.ReadAll(resp.Body); return b }(), &rel); err != nil {
		return
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	d.mu.Lock()
	d.latestVersion = latest
	d.mu.Unlock()
	if latest != "" && latest != strings.TrimPrefix(d.version, "v") && d.version != "dev" {
		log.Printf("⚠ update available: %s → %s (run: agentnet version)", d.version, latest)
	}
}

// connectAndRejoin connects to the relay and rejoins previously joined rooms.
func (d *Daemon) connectAndRejoin() error {
	c, err := client.Connect(d.relay, d.keys.AgentID(), d.agentName, d.keys.PrivateKey)
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.client = c
	rooms := make([]string, 0, len(d.joinedRooms))
	for room := range d.joinedRooms {
		rooms = append(rooms, room)
	}
	d.mu.Unlock()

	// Re-join rooms from previous session
	for _, room := range rooms {
		if _, err := c.JoinRoom(room); err != nil {
			log.Printf("rejoin %s: %v", room, err)
		} else {
			log.Printf("rejoined room: %s", room)
		}
	}

	go d.collectMessages(c)
	return nil
}

// reconnectLoop watches for disconnection and reconnects with exponential backoff.
func (d *Daemon) reconnectLoop() {
	for {
		d.mu.RLock()
		c := d.client
		d.mu.RUnlock()

		// Wait until client disconnects
		if c != nil {
			c.Wait()
		}

		d.mu.Lock()
		d.client = nil
		d.mu.Unlock()

		log.Printf("relay disconnected, reconnecting...")

		// Exponential backoff: 2s, 4s, 8s, ... up to 60s
		backoff := 2 * time.Second
		for {
			time.Sleep(backoff)
			log.Printf("attempting reconnect to %s...", d.relay)
			if err := d.connectAndRejoin(); err != nil {
				log.Printf("reconnect failed: %v", err)
				if backoff < 60*time.Second {
					backoff *= 2
				}
				continue
			}
			log.Printf("reconnected successfully")
			break
		}
	}
}

func (d *Daemon) collectMessages(c *client.Client) {
	for msg := range c.Messages() {
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
	latest := d.latestVersion
	d.mu.RUnlock()

	current := strings.TrimPrefix(d.version, "v")
	updateAvailable := latest != "" && latest != current && d.version != "dev"

	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":        connected,
		"relay":            d.relay,
		"agent_name":       d.agentName,
		"version":          d.version,
		"latest_version":   latest,
		"update_available": updateAvailable,
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
	d.mu.Lock()
	d.joinedRooms[req.Room] = true
	d.mu.Unlock()
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
	d.mu.Lock()
	d.joinedRooms[req.Room] = true
	d.mu.Unlock()
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
	d.mu.Lock()
	delete(d.joinedRooms, req.Room)
	d.mu.Unlock()
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

	d.mu.Lock()
	var msgs []client.IncomingMessage
	var remaining []client.IncomingMessage
	for _, m := range d.messages {
		if roomFilter == "" || strings.EqualFold(m.Room, roomFilter) {
			msgs = append(msgs, m)
		} else {
			remaining = append(remaining, m)
		}
	}
	// Clear returned messages from buffer, keep unrelated rooms
	d.messages = remaining
	d.mu.Unlock()

	// Return last 50
	if len(msgs) > 50 {
		msgs = msgs[len(msgs)-50:]
	}

	json.NewEncoder(w).Encode(msgs)
}

// relayHTTPBase converts a WebSocket relay URL to its HTTP base URL.
// e.g. wss://agentnet.bettalab.me/v1/ws → https://agentnet.bettalab.me
func relayHTTPBase(relayWS string) string {
	s := relayWS
	scheme := "https"
	if strings.HasPrefix(s, "wss://") {
		s = strings.TrimPrefix(s, "wss://")
	} else if strings.HasPrefix(s, "ws://") {
		s = strings.TrimPrefix(s, "ws://")
		scheme = "http"
	}
	// Strip path, keep only host[:port]
	if i := strings.Index(s, "/"); i != -1 {
		s = s[:i]
	}
	return scheme + "://" + s
}

// RelayMessage is the shape returned by the relay's REST message API.
type RelayMessage struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	AgentID   string `json:"from_id"`
	AgentName string `json:"from_name"`
	Content   string `json:"content"` // JSON string: {"type":"text","text":"..."}
	Timestamp int64  `json:"timestamp"` // milliseconds
}

// parseRelayContent extracts plain text from relay content JSON.
func parseRelayContent(content string) string {
	var c struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return content // fall back to raw
	}
	if c.Text != "" {
		return c.Text
	}
	return content
}

func (d *Daemon) handleHistory(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	if room == "" {
		http.Error(w, "room parameter required", http.StatusBadRequest)
		return
	}
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "20"
	}

	base := relayHTTPBase(d.relay)
	url := fmt.Sprintf("%s/api/rooms/%s/messages?limit=%s", base, room, limit)

	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("relay unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, fmt.Sprintf("relay error %d: %s", resp.StatusCode, body), resp.StatusCode)
		return
	}

	var envelope struct {
		Messages []RelayMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		http.Error(w, "failed to decode relay response", http.StatusInternalServerError)
		return
	}
	msgs := envelope.Messages

	// Format as human-readable text for LLM consumption
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "=== Room: %s (last %s messages) ===\n", room, limit)
	if len(msgs) == 0 {
		fmt.Fprintln(w, "(no messages)")
		return
	}
	for _, m := range msgs {
		ts := time.UnixMilli(m.Timestamp).UTC().Format("2006-01-02 15:04:05")
		name := m.AgentName
		if name == "" {
			name = m.AgentID
		}
		text := parseRelayContent(m.Content)
		fmt.Fprintf(w, "[%s] %s: %s\n", ts, name, text)
	}
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
