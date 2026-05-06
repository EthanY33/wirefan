package conn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestConnectedMessageSent(t *testing.T) {
	var got map[string]any
	var gotMu sync.Mutex

	handler := func(c *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = Run(ctx, c, "01HTEST")
	}

	srv := httptest.NewServer(websocketHandler(handler))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	_, data, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gotMu.Lock()
	json.Unmarshal(data, &got)
	gotMu.Unlock()

	if got["type"] != "connected" || got["socket_id"] != "01HTEST" {
		t.Fatalf("got %+v", got)
	}
}

// websocketHandler accepts a WS upgrade and passes the connected websocket.Conn to fn.
// Used to test conn-level behavior with a real upgraded conn rather than mocking.
func websocketHandler(fn func(*websocket.Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		fn(c)
	})
}
