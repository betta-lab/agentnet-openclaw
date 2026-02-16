package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/betta-lab/agentnet-openclaw/internal/client"
)

func TestAuth_MissingToken(t *testing.T) {
	d := &Daemon{apiToken: "secret"}

	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuth_WrongToken(t *testing.T) {
	d := &Daemon{apiToken: "secret"}

	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuth_ValidToken(t *testing.T) {
	d := &Daemon{apiToken: "secret"}

	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestStatus_NotConnected(t *testing.T) {
	d := &Daemon{
		apiToken:  "tok",
		agentName: "Test",
		relay:     "wss://example.com/v1/ws",
	}

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleStatus(w, req)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["connected"] != false {
		t.Fatalf("expected connected=false, got %v", resp["connected"])
	}
	if resp["relay"] != "wss://example.com/v1/ws" {
		t.Fatalf("relay mismatch: %v", resp["relay"])
	}
}

func TestSend_NotConnected(t *testing.T) {
	d := &Daemon{apiToken: "tok"}

	body := strings.NewReader(`{"room":"test","text":"hello"}`)
	req := httptest.NewRequest("POST", "/send", body)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleSend(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not connected, got %d", w.Code)
	}
}

func TestSend_MethodNotAllowed(t *testing.T) {
	d := &Daemon{apiToken: "tok"}

	req := httptest.NewRequest("GET", "/send", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleSend(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestJoinRoom_NotConnected(t *testing.T) {
	d := &Daemon{apiToken: "tok"}

	body := strings.NewReader(`{"room":"test"}`)
	req := httptest.NewRequest("POST", "/rooms/join", body)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleJoinRoom(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestCreateRoom_NotConnected(t *testing.T) {
	d := &Daemon{apiToken: "tok"}

	body := strings.NewReader(`{"room":"test","topic":"t","tags":["a"]}`)
	req := httptest.NewRequest("POST", "/rooms/create", body)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleCreateRoom(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestLeaveRoom_NotConnected(t *testing.T) {
	d := &Daemon{apiToken: "tok"}

	body := strings.NewReader(`{"room":"test"}`)
	req := httptest.NewRequest("POST", "/rooms/leave", body)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleLeaveRoom(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestMessages_Empty(t *testing.T) {
	d := &Daemon{
		apiToken: "tok",
		messages: make([]client.IncomingMessage, 0),
	}

	req := httptest.NewRequest("GET", "/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleMessages(w, req)

	var msgs []interface{}
	json.NewDecoder(w.Body).Decode(&msgs)

	// null or empty array is fine
	if msgs != nil && len(msgs) > 0 {
		t.Fatalf("expected empty messages, got %d", len(msgs))
	}
}

func TestMessages_RoomFilter(t *testing.T) {
	d := &Daemon{
		apiToken: "tok",
		messages: []client.IncomingMessage{
			{Room: "room-a", From: "a1", Text: "hello from a"},
			{Room: "room-b", From: "b1", Text: "hello from b"},
			{Room: "room-a", From: "a2", Text: "another from a"},
		},
	}

	req := httptest.NewRequest("GET", "/messages?room=room-a", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleMessages(w, req)

	var msgs []client.IncomingMessage
	json.NewDecoder(w.Body).Decode(&msgs)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for room-a, got %d", len(msgs))
	}
	for _, m := range msgs {
		if m.Room != "room-a" {
			t.Fatalf("unexpected room: %s", m.Room)
		}
	}
}

func TestCreateRoom_BadRequest(t *testing.T) {
	d := &Daemon{apiToken: "tok", client: nil}

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/rooms/create", body)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handleCreateRoom(w, req)

	// Should get 400 or 503 (not connected), not panic
	if w.Code == 200 {
		t.Fatal("expected non-200 for bad request")
	}
}
