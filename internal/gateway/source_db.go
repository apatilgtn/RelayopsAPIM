package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/relayops/apim/internal/store"
)

// SnapshotSource is where a node gets the configuration it serves. Combined
// and control-plane nodes read Postgres directly (DBSource); gateway-only nodes
// pull from the control plane's node API instead (dataplane.RemoteSource).
type SnapshotSource interface {
	// Load returns the snapshot this node should serve.
	Load(ctx context.Context, nodeGroup string, isCanary bool) (store.SnapshotData, error)
	// Ack reports the configuration a node applied. It doubles as a heartbeat.
	Ack(ctx context.Context, ack NodeAck) error
	// Watch blocks, calling notify whenever the configuration may have changed,
	// and returns when the change feed disconnects (the caller reconnects).
	Watch(ctx context.Context, notify func(reason string)) error
}

// CacheCodec is implemented by sources that persist their own form of the
// last-known-good cache, such as a signed envelope that is verified again
// before a cold start serves it. Other sources cache the plain snapshot.
type CacheCodec interface {
	EncodeCache(data store.SnapshotData) ([]byte, error)
	DecodeCache(raw []byte) (store.SnapshotData, error)
}

// NodeAck is what a node reports after applying a configuration.
type NodeAck struct {
	NodeID         string  `json:"node_id"`
	Revision       int64   `json:"revision"`
	CanaryRevision int64   `json:"canary_revision"`
	Routes         int     `json:"routes"`
	Keys           int     `json:"keys"`
	TookMS         float64 `json:"took_ms"`
	NodeGroup      string  `json:"node_group"`
	IsCanary       bool    `json:"is_canary"`
}

// DBSource reads configuration straight from Postgres and follows changes
// through LISTEN/NOTIFY on the config channel.
type DBSource struct {
	store *store.Store
}

// NewDBSource returns a source backed by s. It returns nil for a nil store so
// callers get a nil SnapshotSource (cache-only operation), not a typed nil.
func NewDBSource(s *store.Store) SnapshotSource {
	if s == nil {
		return nil
	}
	return &DBSource{store: s}
}

func (d *DBSource) Load(ctx context.Context, nodeGroup string, isCanary bool) (store.SnapshotData, error) {
	return d.store.LoadSnapshotDataForNode(ctx, nodeGroup, isCanary)
}

func (d *DBSource) Ack(ctx context.Context, a NodeAck) error {
	return d.store.AcknowledgeRevisionWithCanary(ctx, a.NodeID, a.Revision, a.CanaryRevision, a.Routes, a.Keys, a.TookMS, a.NodeGroup, a.IsCanary)
}

func (d *DBSource) Watch(ctx context.Context, notify func(reason string)) error {
	if d.store.Pool == nil {
		return errors.New("database store uninitialized")
	}
	conn, err := d.store.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+store.ConfigChannel); err != nil {
		return err
	}
	slog.Info("listening for config changes", "channel", store.ConfigChannel)
	// We may have missed changes while disconnected.
	notify("listener_connected")
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var payload struct {
			Table string `json:"table"`
			Op    string `json:"op"`
		}
		_ = json.Unmarshal([]byte(n.Payload), &payload)
		notify(payload.Op + " " + payload.Table)
	}
}
