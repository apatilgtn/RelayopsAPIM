package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/store"
)

// pollWait is how long each config long-poll asks the control plane to hold.
var pollWait = 25 * time.Second

// maxIssuedSkew tolerates clock differences between control-plane replicas
// when refusing configurations older than the applied one.
const maxIssuedSkew = time.Minute

// RemoteSource is a gateway.SnapshotSource backed by the control plane's node
// API: a long-poll keyed by the configuration fingerprint replaces LISTEN/NOTIFY.
// With a Verifier it accepts only configurations signed by a trusted key, and
// its on-disk cache is the signed envelope, verified again on a cold start.
type RemoteSource struct {
	c        *Client
	verifier *Verifier // nil: signatures are not checked

	mu           sync.Mutex
	etag         string        // fingerprint of the configuration last applied or stashed
	pending      *openedConfig // fetched by Watch, handed to the next Load
	lastEnvelope []byte        // raw envelope of the applied configuration (the cache)
	lastIssued   time.Time

	onStreams func([]store.StreamService)

	lastSync   atomic.Int64 // unix seconds of the last successful exchange
	syncErrors atomic.Int64
	updates    atomic.Int64 // changed configurations received by the long-poll
}

type openedConfig struct {
	env     *fetchedConfig
	payload *ConfigPayload
}

var (
	_ gateway.SnapshotSource = (*RemoteSource)(nil)
	_ gateway.CacheCodec     = (*RemoteSource)(nil)
)

func NewRemoteSource(c *Client) *RemoteSource { return &RemoteSource{c: c} }

// RequireSignatures makes the source reject configurations (and cached
// configurations) not signed by one of v's keys.
func (s *RemoteSource) RequireSignatures(v *Verifier) { s.verifier = v }

// OnStreams registers a callback that receives the TCP stream services each
// time a configuration is applied.
func (s *RemoteSource) OnStreams(fn func([]store.StreamService)) { s.onStreams = fn }

// open verifies an envelope. With signatures required it also refuses an
// envelope issued well before the applied one, which a replay would be.
func (s *RemoteSource) open(f *fetchedConfig) (*openedConfig, error) {
	p, err := f.Open(s.verifier)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	last := s.lastIssued
	s.mu.Unlock()
	if s.verifier != nil && !last.IsZero() && f.IssuedAt.Before(last.Add(-maxIssuedSkew)) {
		return nil, fmt.Errorf("configuration issued at %s predates the applied one (%s); refusing a possible replay",
			f.IssuedAt.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	}
	return &openedConfig{env: f, payload: p}, nil
}

func (s *RemoteSource) apply(o *openedConfig) store.SnapshotData {
	s.mu.Lock()
	s.lastEnvelope = o.env.raw
	if o.env.IssuedAt.After(s.lastIssued) {
		s.lastIssued = o.env.IssuedAt
	}
	s.mu.Unlock()
	if s.onStreams != nil {
		s.onStreams(o.payload.Streams)
	}
	return o.payload.Data
}

// Load returns the configuration stashed by Watch, or fetches it.
func (s *RemoteSource) Load(ctx context.Context, _ string, _ bool) (store.SnapshotData, error) {
	s.mu.Lock()
	o := s.pending
	s.pending = nil
	s.mu.Unlock()
	if o == nil {
		f, err := s.c.fetchConfig(ctx, "", 0)
		if err == nil {
			o, err = s.open(f)
		}
		if err != nil {
			s.syncErrors.Add(1)
			return store.SnapshotData{}, err
		}
		s.mu.Lock()
		s.etag = f.Fingerprint
		s.mu.Unlock()
		s.lastSync.Store(time.Now().Unix())
	}
	return s.apply(o), nil
}

func (s *RemoteSource) Ack(ctx context.Context, ack gateway.NodeAck) error {
	err := s.c.postJSON(ctx, PathAck, ack, nil, false)
	if err != nil {
		s.syncErrors.Add(1)
		return err
	}
	s.lastSync.Store(time.Now().Unix())
	return nil
}

// Watch long-polls for configuration changes until a request fails or a
// configuration fails verification.
func (s *RemoteSource) Watch(ctx context.Context, notify func(reason string)) error {
	for {
		s.mu.Lock()
		etag := s.etag
		s.mu.Unlock()
		f, err := s.c.fetchConfig(ctx, etag, pollWait)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.syncErrors.Add(1)
			return err
		}
		s.lastSync.Store(time.Now().Unix())
		if f == nil {
			continue // unchanged
		}
		o, err := s.open(f)
		if err != nil {
			s.syncErrors.Add(1)
			return fmt.Errorf("configuration rejected: %w", err)
		}
		s.mu.Lock()
		s.pending = o
		s.etag = f.Fingerprint
		s.mu.Unlock()
		s.updates.Add(1)
		notify("dataplane_update")
	}
}

// EncodeCache returns the signed envelope of the applied configuration.
func (s *RemoteSource) EncodeCache(store.SnapshotData) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastEnvelope == nil {
		return nil, errors.New("no configuration envelope applied yet")
	}
	return append([]byte(nil), s.lastEnvelope...), nil
}

// DecodeCache verifies a cached envelope (its signature too, when signatures
// are required) before a cold start serves it. Without a verifier it also
// accepts a plain snapshot written before envelopes were cached.
func (s *RemoteSource) DecodeCache(raw []byte) (store.SnapshotData, error) {
	f, err := parseEnvelope(raw)
	if err != nil {
		if s.verifier != nil {
			return store.SnapshotData{}, fmt.Errorf("cached configuration is not a signed envelope: %w", err)
		}
		var d store.SnapshotData
		if jerr := json.Unmarshal(raw, &d); jerr != nil {
			return store.SnapshotData{}, err
		}
		return d, nil
	}
	o, err := s.open(f)
	if err != nil {
		return store.SnapshotData{}, fmt.Errorf("cached configuration rejected: %w", err)
	}
	return s.apply(o), nil
}

// LastSync is when the control plane last answered, or the zero time.
func (s *RemoteSource) LastSync() time.Time {
	if t := s.lastSync.Load(); t > 0 {
		return time.Unix(t, 0)
	}
	return time.Time{}
}

// Updates counts changed configurations received through the long-poll.
func (s *RemoteSource) Updates() int64 { return s.updates.Load() }

// SyncErrors counts failed exchanges with the control plane.
func (s *RemoteSource) SyncErrors() int64 { return s.syncErrors.Load() }

// LogWriter ships request-log batches to the control plane. It plugs into the
// analytics collector, so its spool and replay cover control-plane outages
// exactly as they cover database outages in combined mode.
type LogWriter struct{ c *Client }

func NewLogWriter(c *Client) *LogWriter { return &LogWriter{c: c} }

func (w *LogWriter) InsertLogs(ctx context.Context, logs []store.RequestLog) error {
	if len(logs) == 0 {
		return nil
	}
	return w.c.postJSON(ctx, PathLogs, logs, nil, true)
}

// Ledger is a gateway.AIBudgetLedger served by the control plane. Errors are
// returned, never swallowed, so AI routes keep failing closed when the ledger
// cannot be reached.
type Ledger struct{ c *Client }

var _ gateway.AIBudgetLedger = (*Ledger)(nil)

func NewLedger(c *Client) *Ledger { return &Ledger{c: c} }

// ledgerTimeout bounds each per-request ledger call on the hot path.
const ledgerTimeout = 3 * time.Second

func (l *Ledger) call(ctx context.Context, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
	defer cancel()
	err := l.c.postJSON(ctx, path, in, out, false)
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case CodeNotFound:
			return store.ErrNotFound
		case CodeBudgetExceeded:
			return store.ErrBudgetExceeded
		}
	}
	return err
}

func (l *Ledger) GetAIBudgetAccountByConsumer(ctx context.Context, tenantID, consumerID string) (*store.AIBudgetAccount, error) {
	var acc store.AIBudgetAccount
	if err := l.call(ctx, PathBudgetLookup, BudgetLookupRequest{TenantID: tenantID, ConsumerID: consumerID}, &acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

func (l *Ledger) ReserveAIBudget(ctx context.Context, accountID, requestID string, reserveCents int64) (*store.AIBudgetReservation, error) {
	var res store.AIBudgetReservation
	if err := l.call(ctx, PathBudgetReserve, BudgetReserveRequest{AccountID: accountID, RequestID: requestID, ReserveCents: reserveCents}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (l *Ledger) SettleAIBudgetUsage(ctx context.Context, requestID string, usage store.AIUsage) (int64, error) {
	var out BudgetSettleResponse
	if err := l.call(ctx, PathBudgetSettle, BudgetSettleRequest{RequestID: requestID, Usage: usage}, &out); err != nil {
		return 0, err
	}
	return out.Cents, nil
}

func (l *Ledger) ReleaseAIBudget(ctx context.Context, requestID string) error {
	return l.call(ctx, PathBudgetRelease, BudgetReleaseRequest{RequestID: requestID}, nil)
}
