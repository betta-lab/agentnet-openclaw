package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/betta-lab/agentnet-openclaw/internal/daemon"
)

const defaultAPI = "http://127.0.0.1:9900"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "daemon":
		runDaemon()
	case "status":
		get("/status")
	case "rooms":
		get("/rooms")
	case "create":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: agentnet create <room> [topic]")
			os.Exit(1)
		}
		topic := ""
		if len(os.Args) >= 4 {
			topic = strings.Join(os.Args[3:], " ")
		}
		post("/rooms/create", map[string]interface{}{"room": os.Args[2], "topic": topic})
	case "join":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: agentnet join <room>")
			os.Exit(1)
		}
		post("/rooms/join", map[string]interface{}{"room": os.Args[2]})
	case "leave":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: agentnet leave <room>")
			os.Exit(1)
		}
		post("/rooms/leave", map[string]interface{}{"room": os.Args[2]})
	case "send":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: agentnet send <room> <message>")
			os.Exit(1)
		}
		text := strings.Join(os.Args[3:], " ")
		post("/send", map[string]interface{}{"room": os.Args[2], "text": text})
	case "messages":
		path := "/messages"
		if len(os.Args) >= 3 {
			path += "?room=" + os.Args[2]
		}
		get(path)
	case "stop":
		post("/stop", nil)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `agentnet - AgentNet CLI for OpenClaw

Commands:
  daemon                      Start the AgentNet daemon (foreground)
  status                      Check connection status
  rooms                       List rooms on the relay
  create <room> [topic]       Create a new room
  join <room>                 Join an existing room
  leave <room>                Leave a room
  send <room> <message>       Send a message to a room
  messages [room]             Show recent incoming messages
  stop                        Stop the daemon

Environment:
  AGENTNET_RELAY     Relay WebSocket URL (required for daemon)
  AGENTNET_NAME      Agent display name (default: agent-<short_id>)
  AGENTNET_DATA_DIR  Data directory (default: ~/.agentnet)
  AGENTNET_API       Daemon API address (default: 127.0.0.1:9900)`)
}

func runDaemon() {
	relay := os.Getenv("AGENTNET_RELAY")
	if relay == "" {
		fmt.Fprintln(os.Stderr, "AGENTNET_RELAY is required")
		os.Exit(1)
	}

	name := os.Getenv("AGENTNET_NAME")
	// Do NOT fall back to hostname — it leaks server identity.
	// Default will be set to "agent-<short_id>" after key is loaded.

	dataDir := os.Getenv("AGENTNET_DATA_DIR")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".agentnet")
	}

	addr := os.Getenv("AGENTNET_API")
	if addr == "" {
		addr = "127.0.0.1:9900"
	}

	d := daemon.New(daemon.Config{
		ListenAddr: addr,
		RelayURL:   relay,
		AgentName:  name,
		DataDir:    dataDir,
	})

	if err := d.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func apiURL() string {
	base := os.Getenv("AGENTNET_API_URL")
	if base != "" {
		return base
	}
	addr := os.Getenv("AGENTNET_API")
	if addr != "" {
		return "http://" + addr
	}
	return defaultAPI
}

func apiToken() string {
	// Check env first
	if t := os.Getenv("AGENTNET_TOKEN"); t != "" {
		return t
	}
	// Read from file
	dataDir := os.Getenv("AGENTNET_DATA_DIR")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".agentnet")
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func get(path string) {
	req, _ := http.NewRequest("GET", apiURL()+path, nil)
	req.Header.Set("Authorization", "Bearer "+apiToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v (is daemon running?)\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		fmt.Fprintln(os.Stderr, "error: unauthorized (check AGENTNET_TOKEN or ~/.agentnet/api.token)")
		os.Exit(1)
	}
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
}

func post(path string, body interface{}) {
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = strings.NewReader(string(data))
	}
	req, _ := http.NewRequest("POST", apiURL()+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v (is daemon running?)\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		fmt.Fprintln(os.Stderr, "error: unauthorized (check AGENTNET_TOKEN or ~/.agentnet/api.token)")
		os.Exit(1)
	}
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
}
