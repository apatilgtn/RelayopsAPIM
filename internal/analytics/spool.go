package analytics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/store"
)

// Request logs are the evidence behind automatic rollback, diagnosis and
// analytics, so they must survive database outages. Batches that cannot be
// written to Postgres are appended to a bounded per-node spool file (JSON
// lines) and replayed, oldest first, once the database is reachable again.
//
// Delivery is at-least-once: if Postgres commits a batch but the
// acknowledgement is lost, that batch can be written twice.

// LogWriter is the subset of the store the collector persists through.
type LogWriter interface {
	InsertLogs(ctx context.Context, logs []store.RequestLog) error
}

type logWriter = LogWriter

type spool struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	size     atomic.Int64
	records  atomic.Int64

	spooled     atomic.Int64 // records ever written to the spool
	replayed    atomic.Int64 // records replayed into Postgres
	droppedFull atomic.Int64 // records lost because the spool was full
}

func openSpool(path string, maxBytes int64) (*spool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &spool{path: path, maxBytes: maxBytes}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	s.size.Store(st.Size())
	n, _ := countLines(f)
	s.records.Store(n)
	return s, nil
}

func countLines(r io.Reader) (int64, error) {
	var n int64
	buf := make([]byte, 64<<10)
	for {
		c, err := r.Read(buf)
		n += int64(bytes.Count(buf[:c], []byte{'\n'}))
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// append writes records until the spool is full; the rest are counted as dropped.
func (s *spool) append(logs []store.RequestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.droppedFull.Add(int64(len(logs)))
		slog.Error("request log spool unavailable; logs dropped", "err", err, "count", len(logs))
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	written := 0
	for _, l := range logs {
		line, err := json.Marshal(l)
		if err != nil {
			continue
		}
		if s.size.Load()+int64(len(line))+1 > s.maxBytes {
			break
		}
		_, _ = w.Write(line)
		_ = w.WriteByte('\n')
		s.size.Add(int64(len(line)) + 1)
		written++
	}
	if err := w.Flush(); err != nil {
		slog.Error("request log spool write failed", "err", err)
	}
	s.records.Add(int64(written))
	s.spooled.Add(int64(written))
	if dropped := len(logs) - written; dropped > 0 {
		s.droppedFull.Add(int64(dropped))
		slog.Error("request log spool full; logs dropped", "count", dropped, "max_bytes", s.maxBytes)
	}
}

// replay streams spooled records to Postgres in order, one batch at a time, so
// memory stays bounded by the batch size however large the spool is. On failure
// the unsent remainder stays in the spool. It reports whether the spool is now empty.
func (s *spool) replay(ctx context.Context, w logWriter, batch int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records.Load() == 0 {
		return true, nil
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.records.Store(0)
		s.size.Store(0)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)

	var committed int64 // byte offset just past the last record written to Postgres
	var pending []store.RequestLog
	var pendingBytes int64
	var sendErr error
	flushPending := func() bool {
		if len(pending) == 0 {
			return true
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		sendErr = w.InsertLogs(cctx, pending)
		cancel()
		if sendErr != nil {
			return false
		}
		s.replayed.Add(int64(len(pending)))
		s.records.Add(-int64(len(pending)))
		committed += pendingBytes
		pending, pendingBytes = pending[:0], 0
		return true
	}
	for sc.Scan() {
		line := sc.Bytes()
		pendingBytes += int64(len(line)) + 1
		var l store.RequestLog
		if json.Unmarshal(line, &l) == nil {
			pending = append(pending, l)
		}
		if len(pending) >= batch && !flushPending() {
			break
		}
	}
	if sendErr == nil {
		if err := sc.Err(); err != nil {
			f.Close()
			return false, err
		}
		flushPending()
	}

	if sendErr == nil {
		f.Close()
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		s.records.Store(0)
		s.size.Store(0)
		return true, nil
	}

	// Keep everything after the last committed record.
	if _, err := f.Seek(committed, io.SeekStart); err != nil {
		f.Close()
		return false, err
	}
	tmp := s.path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		f.Close()
		return false, err
	}
	n, copyErr := io.Copy(out, f)
	f.Close()
	if cerr := out.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		return false, copyErr
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return false, err
	}
	s.size.Store(n)
	if cnt, err := countFileLines(s.path); err == nil {
		s.records.Store(cnt)
	}
	return false, sendErr
}

func countFileLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return countLines(f)
}
