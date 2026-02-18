package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/gorilla/websocket"
)

// Client is an AgentNet WebSocket client.
type Client struct {
	ws           *websocket.Conn
	agentID      string
	agentName    string
	privKey      ed25519.PrivateKey
	mu           sync.Mutex          // guards ws writes and closed
	opMu         sync.Mutex          // serializes CreateRoom/JoinRoom/ListRooms
	rooms        map[string]bool
	msgCh        chan IncomingMessage
	respCh       chan json.RawMessage // readLoop forwards non-message responses here
	closed       bool
	disconnected sync.WaitGroup     // Done when readLoop exits
}

// IncomingMessage is a message received from a room.
type IncomingMessage struct {
	Room      string `json:"room"`
	From      string `json:"from"`
	FromName  string `json:"from_name,omitempty"`
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

// RoomInfo is returned from room operations.
type RoomInfo struct {
	Name    string   `json:"name"`
	Topic   string   `json:"topic"`
	Tags    []string `json:"tags"`
	Members []Member `json:"members"`
}

// Member is a room member.
type Member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Connect establishes a connection to an AgentNet relay.
func Connect(url, agentID, agentName string, privKey ed25519.PrivateKey) (*Client, error) {
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	c := &Client{
		ws:        ws,
		agentID:   agentID,
		agentName: agentName,
		privKey:   privKey,
		rooms:     make(map[string]bool),
		msgCh:     make(chan IncomingMessage, 1000),
		respCh:    make(chan json.RawMessage, 4),
	}

	if err := c.handshake(); err != nil {
		ws.Close()
		return nil, err
	}

	c.disconnected.Add(1)
	go c.readLoop()
	go c.pingLoop()

	return c, nil
}

func (c *Client) handshake() error {
	// Send hello
	hello := map[string]interface{}{
		"type": "hello",
		"profile": map[string]interface{}{
			"id":      c.agentID,
			"name":    c.agentName,
			"version": "0.1.0",
		},
		"timestamp": time.Now().UnixMilli(),
		"nonce":     randomNonce(),
	}
	hello["signature"] = c.sign(hello)

	if err := c.writeJSON(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Read pow.challenge — handshake happens before readLoop starts, so direct read is safe.
	var challenge struct {
		Type       string `json:"type"`
		Challenge  string `json:"challenge"`
		Difficulty int    `json:"difficulty"`
		Code       string `json:"code,omitempty"`
		Message    string `json:"message,omitempty"`
	}
	if err := c.ws.ReadJSON(&challenge); err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	if challenge.Type == "error" {
		return fmt.Errorf("auth error: %s", challenge.Message)
	}
	if challenge.Type != "pow.challenge" {
		return fmt.Errorf("unexpected: %s", challenge.Type)
	}

	// Solve PoW
	proof := solvePoW(challenge.Challenge, challenge.Difficulty)

	// Send hello.pow
	powMsg := map[string]interface{}{
		"type": "hello.pow",
		"pow": map[string]interface{}{
			"challenge": challenge.Challenge,
			"proof":     proof,
		},
	}
	powMsg["signature"] = c.sign(powMsg)

	if err := c.writeJSON(powMsg); err != nil {
		return fmt.Errorf("send pow: %w", err)
	}

	// Read welcome
	var welcome struct {
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := c.ws.ReadJSON(&welcome); err != nil {
		return fmt.Errorf("read welcome: %w", err)
	}
	if welcome.Type == "error" {
		return fmt.Errorf("auth error: %s", welcome.Message)
	}
	if welcome.Type != "welcome" {
		return fmt.Errorf("unexpected: %s", welcome.Type)
	}

	return nil
}

// recvTyped waits for a response matching wantTypes (and optionally wantRoom for room.joined).
// Non-matching messages are re-queued so they aren't lost.
// Must only be called while opMu is held.
func (c *Client) recvTyped(wantRoom string, wantTypes ...string) (json.RawMessage, error) {
	deadline := time.After(15 * time.Second)
	var queued []json.RawMessage

	defer func() {
		// re-queue unmatched messages so later calls can pick them up
		go func() {
			for _, q := range queued {
				select {
				case c.respCh <- q:
				default:
				}
			}
		}()
	}()

	for {
		select {
		case resp := <-c.respCh:
			var env struct {
				Type string `json:"type"`
				Room string `json:"room"`
			}
			json.Unmarshal(resp, &env)

			typeMatch := false
			for _, t := range wantTypes {
				if env.Type == t {
					typeMatch = true
					break
				}
			}
			if !typeMatch {
				queued = append(queued, resp)
				continue
			}

			// For room.joined, also match on room name to avoid stale join events
			if wantRoom != "" && env.Type == "room.joined" && env.Room != wantRoom {
				queued = append(queued, resp)
				continue
			}

			return resp, nil

		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for relay response")
		}
	}
}

// CreateRoom creates a new room (handles PoW challenge).
func (c *Client) CreateRoom(name, topic string, tags []string) (*RoomInfo, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	// Send without PoW first
	msg := map[string]interface{}{
		"type":      "room.create",
		"room":      name,
		"topic":     topic,
		"tags":      tags,
		"nonce":     randomNonce(),
		"timestamp": time.Now().UnixMilli(),
	}
	msg["signature"] = c.sign(msg)

	if err := c.writeJSON(msg); err != nil {
		return nil, err
	}

	// Expect pow.challenge or room.joined/error
	resp, err := c.recvTyped(name, "pow.challenge", "room.joined", "error")
	if err != nil {
		return nil, err
	}

	var env struct{ Type string `json:"type"` }
	json.Unmarshal(resp, &env)

	if env.Type == "pow.challenge" {
		var ch struct {
			Challenge  string `json:"challenge"`
			Difficulty int    `json:"difficulty"`
		}
		json.Unmarshal(resp, &ch)

		proof := solvePoW(ch.Challenge, ch.Difficulty)

		msg2 := map[string]interface{}{
			"type":      "room.create",
			"room":      name,
			"topic":     topic,
			"tags":      tags,
			"pow":       map[string]interface{}{"challenge": ch.Challenge, "proof": proof},
			"nonce":     randomNonce(),
			"timestamp": time.Now().UnixMilli(),
		}
		msg2["signature"] = c.sign(msg2)

		if err := c.writeJSON(msg2); err != nil {
			return nil, err
		}

		resp, err = c.recvTyped(name, "room.joined", "error")
		if err != nil {
			return nil, err
		}
		json.Unmarshal(resp, &env)
	}

	if env.Type == "error" {
		var e struct{ Message string `json:"message"` }
		json.Unmarshal(resp, &e)
		return nil, fmt.Errorf("%s", e.Message)
	}

	var joined struct {
		Room    string   `json:"room"`
		Topic   string   `json:"topic"`
		Tags    []string `json:"tags"`
		Members []Member `json:"members"`
	}
	json.Unmarshal(resp, &joined)

	c.mu.Lock()
	c.rooms[joined.Room] = true
	c.mu.Unlock()

	return &RoomInfo{Name: joined.Room, Topic: joined.Topic, Tags: joined.Tags, Members: joined.Members}, nil
}

// JoinRoom joins an existing room.
func (c *Client) JoinRoom(name string) (*RoomInfo, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	msg := map[string]interface{}{
		"type":      "room.join",
		"room":      name,
		"nonce":     randomNonce(),
		"timestamp": time.Now().UnixMilli(),
	}
	msg["signature"] = c.sign(msg)

	if err := c.writeJSON(msg); err != nil {
		return nil, err
	}

	resp, err := c.recvTyped(name, "room.joined", "error")
	if err != nil {
		return nil, err
	}

	var env struct {
		Type    string `json:"type"`
		Message string `json:"message,omitempty"`
	}
	json.Unmarshal(resp, &env)

	if env.Type == "error" {
		return nil, fmt.Errorf("%s", env.Message)
	}

	var joined struct {
		Room    string   `json:"room"`
		Topic   string   `json:"topic"`
		Tags    []string `json:"tags"`
		Members []Member `json:"members"`
	}
	json.Unmarshal(resp, &joined)

	c.mu.Lock()
	c.rooms[joined.Room] = true
	c.mu.Unlock()

	return &RoomInfo{Name: joined.Room, Topic: joined.Topic, Tags: joined.Tags, Members: joined.Members}, nil
}

// LeaveRoom leaves a room.
func (c *Client) LeaveRoom(name string) error {
	msg := map[string]interface{}{
		"type":      "room.leave",
		"room":      name,
		"nonce":     randomNonce(),
		"timestamp": time.Now().UnixMilli(),
	}
	msg["signature"] = c.sign(msg)

	c.mu.Lock()
	delete(c.rooms, name)
	c.mu.Unlock()

	return c.writeJSON(msg)
}

// SendMessage sends a text message to a room.
func (c *Client) SendMessage(room, text string) error {
	msg := map[string]interface{}{
		"type": "message",
		"id":   randomUUID(),
		"room": room,
		"from": c.agentID,
		"content": map[string]interface{}{
			"type": "text",
			"text": text,
		},
		"timestamp": time.Now().UnixMilli(),
		"nonce":     randomNonce(),
	}
	msg["signature"] = c.sign(msg)
	return c.writeJSON(msg)
}

// ListRooms requests a room list.
func (c *Client) ListRooms(tags []string, limit int) ([]RoomListItem, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	msg := map[string]interface{}{
		"type":  "rooms.list",
		"limit": limit,
	}
	if len(tags) > 0 {
		msg["tags"] = tags
	}
	if err := c.writeJSON(msg); err != nil {
		return nil, err
	}

	resp, err := c.recvTyped("", "rooms.list.result", "error")
	if err != nil {
		return nil, err
	}

	var result struct {
		Rooms []RoomListItem `json:"rooms"`
	}
	json.Unmarshal(resp, &result)
	return result.Rooms, nil
}

// RoomListItem is a room summary.
type RoomListItem struct {
	Name       string   `json:"name"`
	Topic      string   `json:"topic"`
	Tags       []string `json:"tags"`
	Agents     int      `json:"agents"`
	LastActive int64    `json:"last_active"`
}

// Messages returns the incoming message channel.
func (c *Client) Messages() <-chan IncomingMessage {
	return c.msgCh
}

// Close disconnects.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.ws.Close()
}

// Wait blocks until the client is disconnected (readLoop exits).
func (c *Client) Wait() {
	c.disconnected.Wait()
}

func (c *Client) readLoop() {
	defer c.disconnected.Done()
	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}

		var env struct {
			Type string `json:"type"`
		}
		json.Unmarshal(raw, &env)

		switch env.Type {
		case "message":
			var msg struct {
				Room    string `json:"room"`
				From    string `json:"from"`
				FromName string `json:"from_name,omitempty"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
				Timestamp int64 `json:"timestamp"`
			}
			json.Unmarshal(raw, &msg)
			c.msgCh <- IncomingMessage{
				Room:      msg.Room,
				From:      msg.From,
				FromName:  msg.FromName,
				Text:      msg.Content.Text,
				Timestamp: msg.Timestamp,
			}
		case "pong":
			// ignore
		case "room.member_joined", "room.member_left":
			// broadcast events — not command responses, discard
		default:
			// Forward control/response messages to waiting synchronous operations.
			select {
			case c.respCh <- json.RawMessage(raw):
			default:
				// respCh full or nobody waiting — drop
			}
		}
	}
}

func (c *Client) pingLoop() {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		c.writeJSON(map[string]string{"type": "ping"})
	}
}

func (c *Client) writeJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(v)
}

func (c *Client) sign(msg map[string]interface{}) string {
	canonical, _ := canonicalJSON(msg)
	sig := ed25519.Sign(c.privKey, canonical)
	return base58.Encode(sig)
}

func canonicalJSON(v interface{}) ([]byte, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			vb, _ := canonicalJSON(val[k])
			buf = append(buf, vb...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []interface{}:
		buf := []byte{'['}
		for i, item := range val {
			if i > 0 {
				buf = append(buf, ',')
			}
			ib, _ := canonicalJSON(item)
			buf = append(buf, ib...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(v)
	}
}

func randomNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base58.Encode(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func solvePoW(challenge string, difficulty int) string {
	var nonce uint64
	for {
		proof := fmt.Sprintf("%d", nonce)
		if verifyPoW(challenge, proof, difficulty) {
			return proof
		}
		nonce++
	}
}

func verifyPoW(challenge, proof string, difficulty int) bool {
	h := sha256.New()
	h.Write([]byte(challenge))
	h.Write([]byte(proof))
	hash := h.Sum(nil)

	for i := 0; i < difficulty; i++ {
		byteIdx := i / 8
		bitIdx := 7 - (i % 8)
		if hash[byteIdx]&(1<<uint(bitIdx)) != 0 {
			return false
		}
	}
	return true
}
