package gateway

import (
	"math"
	"sync"
	"time"
)

// Limiter is an in-memory token-bucket rate limiter keyed by an arbitrary
// string (api + caller identity). Capacity == limit/min, refill == limit/60 per second.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	limit  int
	last   time.Time
}

func NewLimiter() *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}}
	go l.janitor()
	return l
}

type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

func (l *Limiter) Allow(key string, limitPerMinute int) Decision {
	if limitPerMinute <= 0 {
		return Decision{Allowed: true}
	}
	now := time.Now()
	rate := float64(limitPerMinute) / 60.0 // tokens per second

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok || b.limit != limitPerMinute {
		b = &bucket{tokens: float64(limitPerMinute), limit: limitPerMinute, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(float64(limitPerMinute), b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return Decision{Allowed: true, Limit: limitPerMinute, Remaining: int(b.tokens)}
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return Decision{Allowed: false, Limit: limitPerMinute, Remaining: 0, RetryAfter: wait}
}

func (l *Limiter) janitor() {
	for range time.Tick(time.Minute) {
		cutoff := time.Now().Add(-5 * time.Minute)
		l.mu.Lock()
		for k, b := range l.buckets {
			if b.last.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}
