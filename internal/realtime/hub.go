// Package realtime provides an in-process pub/sub hub that fans events out to
// Server-Sent-Events subscribers (the live dashboard).
package realtime

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

type Message struct {
	Event string
	Data  []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[chan Message]string // ch -> tenantID (empty string means platform superadmin / all tenants)
	dropped atomic.Int64
}

func NewHub() *Hub {
	return &Hub{clients: make(map[chan Message]string)}
}

// Subscribe registers a subscriber that receives all events across all tenants.
func (h *Hub) Subscribe() chan Message {
	return h.SubscribeTenant("")
}

// SubscribeTenant registers a subscriber that only receives events for the given tenant
// (or global platform events). An empty tenantID receives all events.
func (h *Hub) SubscribeTenant(tenantID string) chan Message {
	ch := make(chan Message, 64)
	h.mu.Lock()
	h.clients[ch] = tenantID
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Message) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Publish delivers a global message to all subscribers.
func (h *Hub) Publish(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := Message{Event: event, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			h.dropped.Add(1)
		}
	}
}

// PublishTenant delivers a message to subscribers scoped to tenantID and to global subscribers.
func (h *Hub) PublishTenant(event string, v any, tenantID string) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := Message{Event: event, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch, clientTenant := range h.clients {
		if clientTenant != "" && clientTenant != tenantID {
			continue
		}
		select {
		case ch <- msg:
		default:
			h.dropped.Add(1)
		}
	}
}

// PublishWithFilter evaluates payload per client based on its tenant subscription.
func (h *Hub) PublishWithFilter(event string, payloadForTenant func(clientTenant string) any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	cache := make(map[string][]byte)
	for ch, clientTenant := range h.clients {
		data, ok := cache[clientTenant]
		if !ok {
			p := payloadForTenant(clientTenant)
			if p == nil {
				continue
			}
			var err error
			data, err = json.Marshal(p)
			if err != nil {
				continue
			}
			cache[clientTenant] = data
		}
		select {
		case ch <- Message{Event: event, Data: data}:
		default:
			h.dropped.Add(1)
		}
	}
}
