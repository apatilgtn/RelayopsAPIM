package dataplane

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/analytics"
)

// PathTicks receives a gateway-only node's one-second traffic ticks, which the
// control plane republishes to the console's live stream.
const PathTicks = "/dataplane/v1/ticks"

// TickForwarder sends a node's live-traffic ticks to the control plane. It
// keeps only the latest tick: when the control plane is slow or down, ticks
// are dropped, never queued, and the request path is never blocked.
type TickForwarder struct {
	c       *Client
	latest  chan analytics.Tick
	dropped atomic.Int64
}

func NewTickForwarder(c *Client) *TickForwarder {
	return &TickForwarder{c: c, latest: make(chan analytics.Tick, 1)}
}

// Forward hands over a tick without blocking; it replaces an unsent one.
func (f *TickForwarder) Forward(t analytics.Tick) {
	select {
	case f.latest <- t:
		return
	default:
	}
	select {
	case <-f.latest: // discard the older, unsent tick
		f.dropped.Add(1)
	default:
	}
	select {
	case f.latest <- t:
	default:
		f.dropped.Add(1)
	}
}

// Run sends ticks until ctx is done.
func (f *TickForwarder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-f.latest:
			sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := f.c.postJSON(sctx, PathTicks, t, nil, false); err != nil {
				f.dropped.Add(1)
			}
			cancel()
		}
	}
}

// Dropped counts ticks that were not delivered.
func (f *TickForwarder) Dropped() int64 { return f.dropped.Load() }
