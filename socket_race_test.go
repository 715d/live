package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSocketCurrentRenderRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := NewHandler()
	h.MountHandler = func(ctx context.Context, s *Socket) (any, error) {
		return 0, nil
	}
	h.RenderHandler = func(ctx context.Context, rc *RenderContext) (io.Reader, error) {
		n, _ := rc.Assigns.(int)
		page := fmt.Sprintf(`<html><head></head><body live-rendered="">%d</body></html>`, n)
		return strings.NewReader(page), nil
	}

	// "inc" is a client event that bumps the counter.
	h.HandleEvent("inc", func(ctx context.Context, s *Socket, p Params) (any, error) {
		n, _ := s.Assigns().(int)
		return n + 1, nil
	})

	// "self-inc" is a server-side self event that also bumps the counter.
	h.HandleSelf("self-inc", func(ctx context.Context, s *Socket, data any) (any, error) {
		n, _ := s.Assigns().(int)
		return n + 1, nil
	})

	e := NewHttpHandler(ctx, h)

	server := httptest.NewServer(e)
	defer server.Close()

	// GET the page to obtain the socket ID cookie.
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	var socketID string
	for _, c := range resp.Cookies() {
		if c.Name == cookieSocketID {
			socketID = c.Value
			break
		}
	}
	if socketID == "" {
		t.Fatal("no socket ID cookie returned")
	}

	// Connect the WebSocket, passing the socket ID as a query parameter.
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "?" + cookieSocketID + "=" + socketID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Wait for the initial "ack" event.
	waitForConnect(t, ctx, conn)

	// Look up the live socket so we can call Self on it.
	sock, err := e.GetSocket(SocketID(socketID))
	if err != nil {
		t.Fatal(err)
	}

	// Continuously drain all server→client messages so the socket's
	// message buffer never fills up (which would block Self calls
	// and the _serveWS write loop).
	drainCtx, drainCancel := context.WithCancel(ctx)
	defer drainCancel()
	go drainMessages(drainCtx, conn, 30*time.Second)

	// Fire client events and self events concurrently.
	// Client events are processed in the WS reader goroutine.
	// Self events are processed in the socket's operate goroutine.
	// Both call RenderSocket → LatestRender / UpdateRender.
	const iterations = 5
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range iterations {
			msg := Event{T: "inc", ID: i + 1}
			data, _ := json.Marshal(msg)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for range iterations {
			if err := sock.Self(ctx, "self-inc", nil); err != nil {
				return
			}
		}
	}()

	wg.Wait()
}

func waitForConnect(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			t.Fatal("websocket did not connect before timeout")
		default:
			_, b, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ev Event
			if err := json.Unmarshal(b, &ev); err != nil {
				t.Fatal(err)
			}
			switch ev.T {
			case EventConnect:
				msg := Event{T: "inc", ID: 1}
				data, _ := json.Marshal(msg)
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					t.Fatal(err)
				}
			default:
				return
			}
		}
	}
}

// drainMessages reads and discards WebSocket messages until the context
// expires or an error occurs.
func drainMessages(ctx context.Context, c *websocket.Conn, d time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}
