package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// Watcher keeps the gateway snapshot in sync with its SnapshotSource. It
// reloads (debounced) on every change notification, plus a periodic full
// resync as a safety net against missed notifications, and re-acknowledges
// the applied configuration as a heartbeat.
type Watcher struct {
	gw      *Gateway
	src     SnapshotSource // nil: serve the on-disk cache only
	hub     *realtime.Hub
	resync  time.Duration
	version atomic.Int64
	trigger chan string
	lastAck atomic.Pointer[NodeAck] // last acknowledgement of a non-cache load

	certMu     sync.Mutex
	certDigest [32]byte  // of the certificate report last delivered
	certSentAt time.Time // when it was delivered
}

// certReportRefresh re-sends an unchanged certificate report now and then,
// so a control plane that lost it (restored database) gets it back.
const certReportRefresh = 10 * time.Minute

// certReportBudget keeps an acknowledgement under the 64 KiB a control plane
// of the previous release accepts.
const certReportBudget = 48 << 10

// ErrCertificatesNotRecorded is returned by SnapshotSource.Ack when the
// acknowledgement was recorded but the attached certificate report was not.
var ErrCertificatesNotRecorded = errors.New("certificate report not recorded")

// sendAck delivers ack with this node's certificate report attached when it
// changed or is due for a refresh, so unchanged reports cost no database
// writes on heartbeats.
func (w *Watcher) sendAck(ctx context.Context, ack NodeAck) error {
	list := certs.Budget(w.gw.certs.Reported(), certReportBudget)
	digest := certs.Digest(list)
	w.certMu.Lock()
	include := digest != w.certDigest || time.Since(w.certSentAt) >= certReportRefresh
	w.certMu.Unlock()
	if include {
		ack.Certificates, ack.CertificatesIncluded = list, true
	}
	err := w.src.Ack(ctx, ack)
	if errors.Is(err, ErrCertificatesNotRecorded) {
		// Acknowledged; the report goes again with the next heartbeat.
		slog.Warn("certificate report not recorded; retrying with the next heartbeat", "err", err)
		return nil
	}
	if err == nil && include {
		w.certMu.Lock()
		w.certDigest, w.certSentAt = digest, time.Now()
		w.certMu.Unlock()
	}
	return err
}

// HeartbeatInterval is how often a node re-acknowledges its applied
// configuration, so fleet status keeps idle nodes that see no config changes.
var HeartbeatInterval = 30 * time.Second

// NewWatcher follows Postgres directly. A nil store means cache-only operation.
func NewWatcher(gw *Gateway, s *store.Store, hub *realtime.Hub, resync time.Duration) *Watcher {
	return NewWatcherWithSource(gw, NewDBSource(s), hub, resync)
}

// NewWatcherWithSource follows an arbitrary configuration source.
func NewWatcherWithSource(gw *Gateway, src SnapshotSource, hub *realtime.Hub, resync time.Duration) *Watcher {
	return &Watcher{gw: gw, src: src, hub: hub, resync: resync, trigger: make(chan string, 1)}
}

// ReloadEvent is published to dashboards whenever config is applied.
type ReloadEvent struct {
	Version int64     `json:"version"`
	Reason  string    `json:"reason"`
	Routes  int       `json:"routes"`
	Keys    int       `json:"keys"`
	Subs    int       `json:"subscriptions"`
	TookMS  float64   `json:"took_ms"`
	Errors  []string  `json:"errors"`
	At      time.Time `json:"at"`

	PolicySource         string `json:"policy_source,omitempty"`
	CanaryRevision       int64  `json:"canary_revision,omitempty"`
	CanaryTrafficPercent int    `json:"canary_traffic_percent,omitempty"`
	CanaryHeader         string `json:"canary_header,omitempty"`
}

const legacyCacheFile = "data/last_known_good_config.json"

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// cacheFile is this node's own last-known-good cache. Nodes sharing a working
// directory must never read each other's cache: a standard node could otherwise
// start serving a canary node's configuration during a database outage.
func (w *Watcher) cacheFile() string {
	return filepath.Join(config.DataDirectory(), "last_known_good_config."+unsafeFileChars.ReplaceAllString(w.gw.NodeID(), "_")+".json")
}

// loadCachedSnapshot reads this node's cache, falling back to the legacy shared
// file only when the node has never written its own (first boot after upgrade).
func (w *Watcher) loadCachedSnapshot() (store.SnapshotData, string, error) {
	path := w.cacheFile()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && os.Getenv("RELAYOPS_ENV") != "production" {
		path = legacyCacheFile
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return store.SnapshotData{}, path, err
	}
	if codec, ok := w.src.(CacheCodec); ok {
		data, err := codec.DecodeCache(raw)
		return data, path, err
	}
	var data store.SnapshotData
	if err := json.Unmarshal(raw, &data); err != nil {
		return store.SnapshotData{}, path, err
	}
	return data, path, nil
}

// encodeCache renders the last-known-good cache for data.
func (w *Watcher) encodeCache(data store.SnapshotData) ([]byte, error) {
	if codec, ok := w.src.(CacheCodec); ok {
		return codec.EncodeCache(data)
	}
	return json.MarshalIndent(data, "", "  ")
}

// Reload loads a fresh snapshot from the source and swaps it in atomically.
// When the source is unreachable a running node keeps its current snapshot; only
// a node with nothing loaded yet (cold start) falls back to its on-disk cache.

func (w *Watcher) Reload(ctx context.Context, reason string) error {
	start := time.Now()
	fromCache := false
	var data store.SnapshotData
	var err error
	if w.src != nil {
		data, err = w.src.Load(ctx, w.gw.NodeGroup(), w.gw.IsCanary())
	} else {
		err = errors.New("configuration source uninitialized")
	}
	if err != nil {
		if w.gw.Snapshot() != nil {
			return fmt.Errorf("load snapshot failed; keeping the current snapshot: %w", err)
		}
		cached, path, cacheErr := w.loadCachedSnapshot()
		if cacheErr != nil {
			return fmt.Errorf("load snapshot failed: %w (no cached config: %v)", err, cacheErr)
		}
		if path == legacyCacheFile {
			slog.Warn("starting from the legacy shared config cache; it may have been written by another node", "path", path)
		}
		data = cached
		fromCache = true
		slog.Warn("gateway operating in cached startup recovery mode from last-known-good config", "revision", data.Revision, "apis", len(data.APIs), "err", err)
	}

	// Atomic: data.Revision is guaranteed consistent with snapshot data from RepeatableRead transaction
	targetRev := data.Revision
	if targetRev <= 0 {
		targetRev = w.version.Add(1)
	} else {
		w.version.Store(targetRev)
	}
	snap, errs := buildSnapshot(data, targetRev, w.gw.upstreams)
	w.gw.swap(snap)

	// Persist last-known-good configuration for disconnected gateway operation resilience.
	// The cache holds key hashes and API auth settings, so only this user may read it.
	if !fromCache {
		if raw, err := w.encodeCache(data); err == nil {
			if err := writeRecoveryCache(w.cacheFile(), raw); err != nil {
				slog.Error("persistent recovery cache write failed", "err", err)
			}
			if os.Getenv("RELAYOPS_ENV") != "production" {
				_ = writeRecoveryCache(legacyCacheFile, raw)
			}
		}
	}
	ev := ReloadEvent{
		Version: snap.Version, Reason: reason, Routes: len(snap.Routes), Keys: len(snap.Keys),
		Subs: len(snap.Subs), TookMS: float64(time.Since(start).Microseconds()) / 1000, Errors: []string{}, At: time.Now(),
		PolicySource: snap.PolicySource,
	}
	if snap.Canary != nil {
		ev.CanaryRevision = snap.Canary.Version
		ev.CanaryTrafficPercent = snap.CanaryRule.TrafficPercent
		ev.CanaryHeader = snap.CanaryRule.Header
	}
	for _, e := range errs {
		ev.Errors = append(ev.Errors, e.Error())
		slog.Warn("config", "err", e)
	}
	slog.Info("config applied", "version", ev.Version, "canary_revision", ev.CanaryRevision, "policy_source", ev.PolicySource,
		"reason", reason, "routes", ev.Routes, "took_ms", ev.TookMS, "from_cache", fromCache)
	w.hub.Publish("config", ev)
	if w.src != nil && !fromCache {
		ack := NodeAck{NodeID: w.gw.NodeID(), Revision: ev.Version, CanaryRevision: ev.CanaryRevision, Routes: ev.Routes,
			Keys: ev.Keys, TookMS: ev.TookMS, NodeGroup: w.gw.NodeGroup(), IsCanary: w.gw.IsCanary()}
		w.lastAck.Store(&ack)
		if err := w.sendAck(ctx, ack); err != nil {
			slog.Warn("config acknowledgement failed", "version", ack.Revision, "err", err)
		}
	}
	return nil
}

func writeRecoveryCache(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// heartbeat re-sends the last acknowledgement so the node stays visible in
// fleet status while its configuration is unchanged.
func (w *Watcher) heartbeat(ctx context.Context) {
	ack := w.lastAck.Load()
	if w.src == nil || ack == nil {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := w.sendAck(hctx, *ack); err != nil {
		slog.Debug("heartbeat failed", "err", err)
	}
}

func (w *Watcher) request(reason string) {
	select {
	case w.trigger <- reason:
	default: // a reload is already pending; it will pick up this change too
	}
}

// Run blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	go w.reloadLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		var err error = errors.New("configuration source uninitialized")
		if w.src != nil {
			err = w.src.Watch(ctx, w.request)
		}
		if ctx.Err() != nil {
			return
		}
		slog.Error("config listener disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *Watcher) reloadLoop(ctx context.Context) {
	tick := time.NewTicker(w.resync)
	defer tick.Stop()
	beat := time.NewTicker(HeartbeatInterval)
	defer beat.Stop()
	for {
		var reason string
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			w.heartbeat(ctx)
			continue
		case reason = <-w.trigger:
			// Debounce bursts (e.g. a bulk import) into a single reload.
			time.Sleep(50 * time.Millisecond)
		case <-tick.C:
			reason = "periodic_resync"
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := w.Reload(rctx, reason); err != nil {
			slog.Error("config reload failed; keeping previous snapshot", "err", err)
		}
		cancel()
	}
}
