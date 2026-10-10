package stream

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func TestStreamProxyBiDirectional(t *testing.T) {
	// 1. Create a dummy TCP backend server (echo server)
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backendLn.Close()

	go func() {
		for {
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c) // echo back
			}(conn)
		}
	}()

	backendAddr := backendLn.Addr().String()

	// 2. Start a StreamService proxy
	mgr := NewManager()
	defer mgr.StopAll()

	// Pick an available port for proxy
	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test port: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()

	srv := store.StreamService{
		ID:               "stream-test-1",
		TenantID:         "tenant-default",
		Name:             "echo-proxy",
		ListenPort:       port,
		TargetAddresses:  []string{backendAddr},
		Enabled:          true,
		MaxConnections:   10,
		ConnectTimeoutMS: 2000,
	}

	if err := mgr.Sync([]store.StreamService{srv}); err != nil {
		t.Fatalf("mgr.Sync: %v", err)
	}

	if mgr.RunningCount() != 1 {
		t.Fatalf("expected 1 running stream listener, got %d", mgr.RunningCount())
	}

	// 3. Connect client to the proxy port and verify echo
	time.Sleep(50 * time.Millisecond)
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()

	payload := []byte("hello relayops stream\n")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}

	if !bytes.Equal(buf, payload) {
		t.Fatalf("expected echoed %q, got %q", string(payload), string(buf))
	}
}
