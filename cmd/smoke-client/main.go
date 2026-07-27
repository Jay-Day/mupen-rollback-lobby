// Smoke-test client for the lobby server. Sends HELLO, prints a few
// presence/chat events, optionally sends one chat line, then exits.
//
// Usage:
//
//	go run ./cmd/smoke-client --user alice --say "hi"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/coder/websocket"
)

type envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

func main() {
	var (
		url       = flag.String("url", "ws://127.0.0.1:8080/ws", "WebSocket URL")
		username  = flag.String("user", "smoke", "Username")
		say       = flag.String("say", "", "Optional chat line to send after HELLO")
		localIP   = flag.String("local-ip", "", "Optional LAN IP advertised in HELLO")
		localPort = flag.Uint("local-port", 0, "Optional LAN UDP port advertised in HELLO")
		listenMs  = flag.Int("listen-ms", 2000, "How long to listen after sending before exiting")
	)
	flag.Parse()
	if *localPort > 65535 {
		log.Fatalf("local-port must be between 0 and 65535")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Send HELLO
	helloData := map[string]any{
		"username":      *username,
		"clientVersion": "smoke-test",
		"romHashes":     []string{},
	}
	if *localIP != "" {
		helloData["localIp"] = *localIP
	}
	if *localPort != 0 {
		helloData["localPort"] = uint16(*localPort)
	}
	hello := envelope{
		Type: "HELLO",
		ID:   "h1",
		Data: jsonRaw(helloData),
	}
	if err := writeJSON(ctx, conn, hello); err != nil {
		log.Fatalf("write hello: %v", err)
	}

	// Optionally send a chat message after a tiny delay so HELLO_OK lands first.
	if *say != "" {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = writeJSON(ctx, conn, envelope{
				Type: "CHAT_SEND",
				Data: jsonRaw(map[string]any{
					"channel": "lobby",
					"message": *say,
				}),
			})
		}()
	}

	deadline := time.Now().Add(time.Duration(*listenMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithDeadline(ctx, deadline)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		var env envelope
		_ = json.Unmarshal(data, &env)
		var prettyData any
		_ = json.Unmarshal(env.Data, &prettyData)
		out, _ := json.MarshalIndent(map[string]any{"type": env.Type, "data": prettyData}, "", "  ")
		fmt.Fprintln(os.Stdout, string(out))
	}
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
