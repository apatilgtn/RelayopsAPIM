package admin

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/realtime"
	"golang.org/x/net/websocket"
)

type hijackableRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw := bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn))
	return h.conn, rw, nil
}

func TestStreamWSTenantFilteringAndHandshake(t *testing.T) {
	hub := realtime.NewHub()
	srv := &Server{
		hub:    hub,
		nodeID: "node-test-1",
	}

	wsHandler := srv.streamWS()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	rec := &hijackableRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
	}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		srvReader := bufio.NewReader(serverConn)
		req, err := http.ReadRequest(srvReader)
		if err != nil {
			return
		}
		wsHandler.ServeHTTP(rec, req)
	}()

	wsConfig, err := websocket.NewConfig("ws://localhost/api/stream/ws?tenant=tenant-alpha", "http://localhost")
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	wsClient, err := websocket.NewClient(wsConfig, clientConn)
	if err != nil {
		t.Fatalf("failed to create ws client: %v", err)
	}
	defer wsClient.Close()

	// 1. Verify "hello" event handshake
	var helloMsg map[string]any
	_ = wsClient.SetDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Receive(wsClient, &helloMsg); err != nil {
		t.Fatalf("failed to receive hello message: %v", err)
	}
	if helloMsg["event"] != "hello" || helloMsg["node"] != "node-test-1" || helloMsg["tenant"] != "tenant-alpha" {
		t.Fatalf("unexpected hello payload: %+v", helloMsg)
	}

	// 2. Publish event for matching tenant-alpha
	hub.PublishTenant("metric", map[string]string{"latency": "12ms"}, "tenant-alpha")

	var alphaMsg struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	_ = wsClient.SetDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Receive(wsClient, &alphaMsg); err != nil {
		t.Fatalf("failed to receive tenant-alpha event: %v", err)
	}
	if alphaMsg.Event != "metric" || !strings.Contains(string(alphaMsg.Data), "12ms") {
		t.Fatalf("unexpected alpha event: %+v", alphaMsg)
	}

	// 3. Publish event for tenant-beta (must NOT be received by tenant-alpha subscriber)
	hub.PublishTenant("secret_metric", map[string]string{"secret": "val"}, "tenant-beta")
	// Publish another tenant-alpha event to assert ordering
	hub.PublishTenant("second_metric", map[string]string{"status": "ok"}, "tenant-alpha")

	var secondMsg struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	_ = wsClient.SetDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Receive(wsClient, &secondMsg); err != nil {
		t.Fatalf("failed to receive second event: %v", err)
	}
	if secondMsg.Event != "second_metric" {
		t.Fatalf("expected second_metric, got event %s (tenant-beta was not filtered!)", secondMsg.Event)
	}

	// 4. Close client connection
	_ = wsClient.Close()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after client connection close")
	}
}
