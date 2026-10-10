package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter handles cluster-wide rate limiting and daily/monthly quotas via Redis,
// falling back to an in-memory limiter if Redis is offline or unconfigured.
type RedisLimiter struct {
	client   *redis.Client
	fallback *Limiter
	hasRedis atomic.Bool
	redisURL string
	mu       sync.RWMutex
}

func NewRedisLimiter(redisURL string, fallback *Limiter) *RedisLimiter {
	rl := &RedisLimiter{fallback: fallback, redisURL: redisURL}
	if redisURL == "" {
		return rl
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Warn("invalid redis url, using in-memory rate limiting", "err", err)
		return rl
	}
	rdb := redis.NewClient(opt)
	rl.client = rdb

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("cannot reach redis on startup, fallback to in-memory (recovery loop active)", "err", err)
	} else {
		slog.Info("connected to redis for cluster rate limiting & quotas", "addr", opt.Addr)
		rl.hasRedis.Store(true)
	}

	// Active health check and automatic recovery loop
	go rl.healthLoop()

	return rl
}

func (rl *RedisLimiter) IsHealthy() bool {
	return rl.hasRedis.Load()
}

func (rl *RedisLimiter) healthLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if rl.client == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := rl.client.Ping(ctx).Err()
		cancel()
		if err == nil {
			if !rl.hasRedis.Swap(true) {
				slog.Info("redis cluster reconnected; rate limiting & quotas restored")
			}
		} else {
			if rl.hasRedis.Swap(false) {
				slog.Warn("redis cluster unreachable; activating degraded policy", "err", err)
			}
		}
	}
}

// slidingWindowScript is a high-performance Redis Lua script that computes a sliding window count.
// KEYS[1] = rate limit key
// ARGV[1] = current timestamp in milliseconds
// ARGV[2] = window size in milliseconds (60000)
// ARGV[3] = max allowed requests in window
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local clearBefore = now - window

redis.call('ZREMRANGEBYSCORE', key, '-inf', clearBefore)
local currentRequests = redis.call('ZCARD', key)

if currentRequests < limit then
    redis.call('ZADD', key, now, now .. '-' .. math.random(1, 1000000))
    redis.call('EXPIRE', key, math.ceil(window / 1000) + 1)
    return {1, limit - currentRequests - 1}
else
    return {0, 0}
end
`)

func (rl *RedisLimiter) Allow(ctx context.Context, key string, limitPerMinute int) Decision {
	if limitPerMinute <= 0 {
		return Decision{Allowed: true, Limit: 0, Remaining: 0}
	}
	if !rl.hasRedis.Load() || rl.client == nil {
		return rl.fallback.Allow(key, limitPerMinute)
	}

	now := time.Now().UnixNano() / int64(time.Millisecond)
	windowMS := int64(60000)
	rKey := "relayops:rl:" + key

	cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	res, err := slidingWindowScript.Run(cctx, rl.client, []string{rKey}, now, windowMS, limitPerMinute).Slice()
	if err != nil {
		// Log and fallback to local limiter if Redis call fails
		slog.Debug("redis rate limit failed, falling back to local", "err", err)
		return rl.fallback.Allow(key, limitPerMinute)
	}

	allowed := res[0].(int64) == 1
	remaining := int(res[1].(int64))

	return Decision{
		Allowed:    allowed,
		Limit:      limitPerMinute,
		Remaining:  remaining,
		RetryAfter: time.Second,
	}
}

// ---------------------------------------------------------------------------
// Quotas (Daily / Monthly)
// ---------------------------------------------------------------------------

type QuotaDecision struct {
	Allowed        bool
	LimitDay       int
	RemainingDay   int
	LimitMonth     int
	RemainingMonth int
	ExceededPeriod string // "day" or "month" or "redis_offline_fail_closed"
	Degraded       bool
}

var quotaScript = redis.NewScript(`
local dayKey = KEYS[1]
local monthKey = KEYS[2]
local limitDay = tonumber(ARGV[1])
local limitMonth = tonumber(ARGV[2])

local nDay = 0
if limitDay > 0 then
    nDay = tonumber(redis.call('GET', dayKey) or "0")
    if nDay >= limitDay then
        return {0, "day", 0, limitMonth > 0 and (limitMonth - tonumber(redis.call('GET', monthKey) or "0")) or -1}
    end
end

local nMonth = 0
if limitMonth > 0 then
    nMonth = tonumber(redis.call('GET', monthKey) or "0")
    if nMonth >= limitMonth then
        return {0, "month", limitDay > 0 and (limitDay - nDay) or -1, 0}
    end
end

nDay = redis.call('INCR', dayKey)
if nDay == 1 then
    redis.call('EXPIRE', dayKey, 172800) -- 48 hours
end

nMonth = redis.call('INCR', monthKey)
if nMonth == 1 then
    redis.call('EXPIRE', monthKey, 5356800) -- 62 days
end

return {1, "ok", limitDay > 0 and (limitDay - nDay) or -1, limitMonth > 0 and (limitMonth - nMonth) or -1}
`)

func (rl *RedisLimiter) CheckQuota(ctx context.Context, key string, quotaDay, quotaMonth int) QuotaDecision {
	return rl.CheckQuotaWithPolicy(ctx, key, quotaDay, quotaMonth, "fail_open")
}

func (rl *RedisLimiter) CheckQuotaWithPolicy(ctx context.Context, key string, quotaDay, quotaMonth int, policy string) QuotaDecision {
	if quotaDay <= 0 && quotaMonth <= 0 {
		return QuotaDecision{Allowed: true}
	}
	if !rl.hasRedis.Load() || rl.client == nil {
		if policy == "fail_closed" {
			return QuotaDecision{
				Allowed:        false,
				LimitDay:       quotaDay,
				RemainingDay:   0,
				LimitMonth:     quotaMonth,
				RemainingMonth: 0,
				ExceededPeriod: "redis_offline_fail_closed",
				Degraded:       true,
			}
		}
		// fail_open
		return QuotaDecision{
			Allowed:        true,
			LimitDay:       quotaDay,
			RemainingDay:   quotaDay,
			LimitMonth:     quotaMonth,
			RemainingMonth: quotaMonth,
			ExceededPeriod: "redis_offline_fail_open",
			Degraded:       true,
		}
	}

	now := time.Now().UTC()
	dayKey := fmt.Sprintf("relayops:quota:day:%s:%s", now.Format("20060102"), key)
	monthKey := fmt.Sprintf("relayops:quota:month:%s:%s", now.Format("200601"), key)

	cctx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
	defer cancel()

	res, err := quotaScript.Run(cctx, rl.client, []string{dayKey, monthKey}, quotaDay, quotaMonth).Slice()
	if err != nil {
		slog.Debug("redis quota check failed", "err", err, "policy", policy)
		if policy == "fail_closed" {
			return QuotaDecision{
				Allowed:        false,
				LimitDay:       quotaDay,
				RemainingDay:   0,
				LimitMonth:     quotaMonth,
				RemainingMonth: 0,
				ExceededPeriod: "redis_offline_fail_closed",
				Degraded:       true,
			}
		}
		return QuotaDecision{
			Allowed:        true,
			LimitDay:       quotaDay,
			RemainingDay:   quotaDay,
			LimitMonth:     quotaMonth,
			RemainingMonth: quotaMonth,
			ExceededPeriod: "redis_offline_fail_open",
			Degraded:       true,
		}
	}

	allowed := res[0].(int64) == 1
	reason := res[1].(string)
	remDay := int(res[2].(int64))
	remMonth := int(res[3].(int64))
	if remDay < 0 {
		remDay = 0
	}
	if remMonth < 0 {
		remMonth = 0
	}

	return QuotaDecision{
		Allowed:        allowed,
		LimitDay:       quotaDay,
		RemainingDay:   remDay,
		LimitMonth:     quotaMonth,
		RemainingMonth: remMonth,
		ExceededPeriod: reason,
		Degraded:       false,
	}
}

func (q QuotaDecision) WriteHeaders(header func(k, v string)) {
	if q.LimitDay > 0 {
		header("X-Quota-Day-Limit", strconv.Itoa(q.LimitDay))
		header("X-Quota-Day-Remaining", strconv.Itoa(q.RemainingDay))
	}
	if q.LimitMonth > 0 {
		header("X-Quota-Month-Limit", strconv.Itoa(q.LimitMonth))
		header("X-Quota-Month-Remaining", strconv.Itoa(q.RemainingMonth))
	}
	if q.Degraded {
		header("X-RelayOps-Degraded", "redis_unavailable")
	}
}
