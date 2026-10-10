package graphql

import (
	"errors"
	"strings"
	"sync"
)

// Automatic persisted queries (APQ): a client sends only the SHA-256 of a
// query it sent before. The gateway must still apply policy to the query,
// so it keeps the queries it has verified; a hash it does not know is
// answered with PersistedQueryNotFound and the client resends the query.

// PersistedQuery is the APQ request extension.
type PersistedQuery struct {
	Version    int    `json:"version"`
	Sha256Hash string `json:"sha256Hash"`
}

// Extensions are a request's GraphQL extensions the gateway understands.
type Extensions struct {
	PersistedQuery *PersistedQuery `json:"persistedQuery,omitempty"`
}

var (
	// ErrPersistedQueryNotFound tells the client to resend the full query.
	ErrPersistedQueryNotFound = errors.New("PersistedQueryNotFound")
	// ErrPersistedQueryMismatch means the query does not hash to the given id.
	ErrPersistedQueryMismatch = errors.New("provided sha256Hash does not match the query")
)

// PersistedQueryNotFoundResponse is the APQ protocol's answer to an unknown
// hash (HTTP 200, so APQ clients retry with the query text).
var PersistedQueryNotFoundResponse = map[string]any{
	"errors": []map[string]any{{
		"message":    "PersistedQueryNotFound",
		"extensions": map[string]any{"code": "PERSISTED_QUERY_NOT_FOUND"},
	}},
}

// PersistedCache holds verified queries by hash, bounded in entries and
// total size; when full, older entries are dropped.
type PersistedCache struct {
	mu      sync.Mutex
	entries map[string]string
	order   []string
	bytes   int
	MaxN    int
	MaxSize int
}

func NewPersistedCache() *PersistedCache {
	return &PersistedCache{entries: map[string]string{}, MaxN: 5000, MaxSize: 32 << 20}
}

func (c *PersistedCache) get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.entries[hash]
	return q, ok
}

func (c *PersistedCache) put(hash, query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[hash]; ok || len(query) > c.MaxSize {
		return
	}
	for len(c.order) > 0 && (len(c.entries) >= c.MaxN || c.bytes+len(query) > c.MaxSize) {
		old := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.entries[old])
		delete(c.entries, old)
	}
	c.entries[hash] = query
	c.order = append(c.order, hash)
	c.bytes += len(query)
}

// ResolvePersisted fills in or verifies a persisted query. Requests without
// the extension are left alone.
func ResolvePersisted(req *Request, cache *PersistedCache) error {
	pq := req.Extensions.PersistedQuery
	if pq == nil || pq.Sha256Hash == "" {
		return nil
	}
	hash := strings.ToLower(pq.Sha256Hash)
	if strings.TrimSpace(req.Query) != "" {
		if QueryHash(req.Query) != "sha256:"+hash {
			return ErrPersistedQueryMismatch
		}
		return nil // cached by Remember once it passes policy
	}
	q, ok := cache.get(hash)
	if !ok {
		return ErrPersistedQueryNotFound
	}
	req.Query = q
	return nil
}

// Remember caches a verified persisted query after it passed policy, so
// later hash-only requests can be checked.
func (c *PersistedCache) Remember(req *Request) {
	pq := req.Extensions.PersistedQuery
	if pq == nil || pq.Sha256Hash == "" || strings.TrimSpace(req.Query) == "" {
		return
	}
	c.put(strings.ToLower(pq.Sha256Hash), req.Query)
}
