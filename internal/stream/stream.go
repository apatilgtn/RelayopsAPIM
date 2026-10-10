package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/store"
)

// ServiceRunner manages the TCP listener and active connections for a single StreamService.
type ServiceRunner struct {
	service    store.StreamService
	listener   net.Listener
	closed     atomic.Bool
	activeConn atomic.Int64
	targetIdx  atomic.Uint64
	cancel     context.CancelFunc
}

// Manager manages a fleet of StreamService TCP listeners.
type Manager struct {
	mu      sync.Mutex
	runners map[string]*ServiceRunner // keyed by service.ID
}

// NewManager creates an empty StreamService manager.
func NewManager() *Manager {
	return &Manager{
		runners: make(map[string]*ServiceRunner),
	}
}

// Sync reconciles active TCP listeners against the desired services list.
func (m *Manager) Sync(services []store.StreamService) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired := make(map[string]store.StreamService)
	for _, s := range services {
		if s.Enabled && len(s.TargetAddresses) > 0 && s.ListenPort > 0 {
			desired[s.ID] = s
		}
	}

	// Stop runners that are no longer enabled or have been removed
	for id, runner := range m.runners {
		target, stillDesired := desired[id]
		if !stillDesired || target.ListenPort != runner.service.ListenPort {
			runner.Stop()
			delete(m.runners, id)
		}
	}

	// Start new or updated runners
	for id, s := range desired {
		if _, exists := m.runners[id]; !exists {
			runner, err := startServiceRunner(s)
			if err != nil {
				return fmt.Errorf("stream service %s (port %d): %w", s.Name, s.ListenPort, err)
			}
			m.runners[id] = runner
		}
	}

	return nil
}

// StopAll shuts down all running stream listeners.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, runner := range m.runners {
		runner.Stop()
		delete(m.runners, id)
	}
}

// RunningCount returns the number of active listeners.
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runners)
}

func startServiceRunner(s store.StreamService) (*ServiceRunner, error) {
	addr := fmt.Sprintf(":%d", s.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	sr := &ServiceRunner{
		service:  s,
		listener: ln,
		cancel:   cancel,
	}

	go sr.acceptLoop(ctx)
	return sr, nil
}

func (sr *ServiceRunner) Stop() {
	if sr.closed.CompareAndSwap(false, true) {
		if sr.cancel != nil {
			sr.cancel()
		}
		if sr.listener != nil {
			_ = sr.listener.Close()
		}
	}
}

func (sr *ServiceRunner) acceptLoop(ctx context.Context) {
	for {
		conn, err := sr.listener.Accept()
		if err != nil {
			if sr.closed.Load() {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}

		maxConns := int64(sr.service.MaxConnections)
		if maxConns <= 0 {
			maxConns = 1000
		}

		if sr.activeConn.Load() >= maxConns {
			_ = conn.Close()
			continue
		}

		sr.activeConn.Add(1)
		go func(c net.Conn) {
			defer sr.activeConn.Add(-1)
			defer c.Close()
			sr.proxyConnection(ctx, c)
		}(conn)
	}
}

func (sr *ServiceRunner) pickTarget() string {
	n := len(sr.service.TargetAddresses)
	if n == 0 {
		return ""
	}
	idx := sr.targetIdx.Add(1) - 1
	return sr.service.TargetAddresses[idx%uint64(n)]
}

func (sr *ServiceRunner) proxyConnection(ctx context.Context, clientConn net.Conn) {
	target := sr.pickTarget()
	if target == "" {
		return
	}

	timeoutMS := sr.service.ConnectTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 5000
	}

	dialer := net.Dialer{Timeout: time.Duration(timeoutMS) * time.Millisecond}
	upstreamConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer upstreamConn.Close()

	// Bi-directional full-duplex TCP pump
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(upstreamConn, clientConn)
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	go func() {
		_, _ = io.Copy(clientConn, upstreamConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}
