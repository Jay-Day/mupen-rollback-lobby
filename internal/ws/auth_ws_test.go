package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// wsTestClient speaks the lobby protocol over a real WebSocket.
type wsTestClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func dialLobby(t *testing.T, url string) *wsTestClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return &wsTestClient{t: t, conn: conn}
}

func (c *wsTestClient) send(msgType string, data any) {
	c.t.Helper()
	payload, _ := json.Marshal(data)
	raw, _ := json.Marshal(protocol.Envelope{Type: msgType, Data: payload})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// await reads until a message of msgType arrives; ok is false if the
// connection closes first.
func (c *wsTestClient) await(msgType string) (protocol.Envelope, bool) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return protocol.Envelope{}, false
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Type == msgType {
			return env, true
		}
	}
}

func (c *wsTestClient) hello(req protocol.HelloRequest) (protocol.HelloOKResponse, *protocol.HelloFailResponse) {
	c.t.Helper()
	req.ClientVersion = "vdev-1"
	c.send(protocol.TypeHello, req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			c.t.Fatalf("read hello reply: %v", err)
		}
		var env protocol.Envelope
		json.Unmarshal(raw, &env)
		switch env.Type {
		case protocol.TypeHelloOK:
			var ok protocol.HelloOKResponse
			json.Unmarshal(env.Data, &ok)
			return ok, nil
		case protocol.TypeHelloFail:
			var fail protocol.HelloFailResponse
			json.Unmarshal(env.Data, &fail)
			return protocol.HelloOKResponse{}, &fail
		}
	}
}

func TestHelloAccountsOverWebSocket(t *testing.T) {
	f := newRankedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.hub.Run(ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hub.ServeWS(w, r, "198.51.100.7")
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	// register: canonical name, token and ladders come back
	first := dialLobby(t, url)
	ok, fail := first.hello(protocol.HelloRequest{Username: "Champ",
		Auth: &protocol.HelloAuth{Mode: "register", Password: "password123"}})
	if fail != nil {
		t.Fatalf("register failed: %+v", fail)
	}
	if ok.Username != "Champ" || ok.Account == nil || ok.Account.SessionToken == "" ||
		len(ok.Ladders) != 1 {
		t.Fatalf("HELLO_OK = %+v", ok)
	}
	// one standing per roster: each is rated separately
	if st := ok.Account.Standings; len(st) != 2 || st[0].Ruleset != "remix-1v1-vanilla" || st[1].Ruleset != "remix-1v1-remix" {
		t.Fatalf("standings = %+v", st)
	}

	// a guest can't use the registered name
	guest := dialLobby(t, url)
	if _, fail := guest.hello(protocol.HelloRequest{Username: "champ"}); fail == nil ||
		fail.Reason != protocol.ReasonUsernameRegistered {
		t.Fatalf("guest hello = %+v", fail)
	}

	// the remember-me token logs in again and takes over the old session
	second := dialLobby(t, url)
	ok2, fail := second.hello(protocol.HelloRequest{Username: "champ",
		Auth: &protocol.HelloAuth{Mode: "token", Token: ok.Account.SessionToken}})
	if fail != nil {
		t.Fatalf("token login failed: %+v", fail)
	}
	if ok2.Username != "Champ" || ok2.Account.AccountID != ok.Account.AccountID {
		t.Fatalf("token HELLO_OK = %+v", ok2)
	}
	if notice, open := first.await(protocol.TypeModNotice); open &&
		!strings.Contains(string(notice.Data), "another location") {
		t.Fatalf("old session notice = %s", notice.Data)
	}
	if _, open := first.await(protocol.TypeHelloOK); open {
		t.Fatal("old session still open after takeover")
	}

	// wrong password
	third := dialLobby(t, url)
	if _, fail := third.hello(protocol.HelloRequest{Username: "Champ",
		Auth: &protocol.HelloAuth{Mode: "login", Password: "nope-nope"}}); fail == nil ||
		fail.Reason != protocol.ReasonAuthFailed {
		t.Fatalf("bad password hello = %+v", fail)
	}
}
