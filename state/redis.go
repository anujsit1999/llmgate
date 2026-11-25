package state

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/anujsit1999/llmgate/config"
)

func init() {
	Register("redis", func(ctx context.Context, cfg config.State) (Store, error) { return NewRedis(ctx, cfg.Redis) })
}

// Redis is a Store shared by every replica.
//
// Each counter is a hash per fixed window: <prefix>:{<key>}:<window-index>.
// The {braces} are a Redis Cluster hash tag, so a key's current and previous
// windows live in the same slot and the Lua script can touch both atomically.
// Windows expire on their own after two minutes.
//
// Window boundaries come from the gateway's clock; keep replicas NTP-synced
// (standard on Kubernetes nodes).
type Redis struct {
	rdb    redis.UniversalClient
	prefix string
	now    func() time.Time
}

// NewRedis connects and verifies the connection (fail fast at startup).
func NewRedis(ctx context.Context, c config.Redis) (*Redis, error) {
	opts := &redis.UniversalOptions{
		Addrs:        c.Addrs(),
		Password:     c.Password(),
		DB:           c.DB,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond, // counters are on the hot path: fail fast
		WriteTimeout: 500 * time.Millisecond,
	}
	if c.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	rdb := redis.NewUniversalClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("state: redis ping: %w", err)
	}
	prefix := c.KeyPrefix
	if prefix == "" {
		prefix = "llmgate"
	}
	return &Redis{rdb: rdb, prefix: prefix, now: time.Now}, nil
}

// keys returns the current and previous window keys for key.
func (s *Redis) keys(key string, now time.Time) (cur, prev string) {
	idx := windowStart(now).Unix() / int64(Window/time.Second)
	base := s.prefix + ":{" + key + "}:"
	return base + strconv.FormatInt(idx, 10), base + strconv.FormatInt(idx-1, 10)
}

const ttl = 2*Window + 10*time.Second

// Add implements Store.
func (s *Redis) Add(ctx context.Context, key string, deltas map[string]float64) error {
	cur, _ := s.keys(key, s.now())
	pipe := s.rdb.Pipeline()
	for f, d := range deltas {
		pipe.HIncrByFloat(ctx, cur, f, d)
	}
	pipe.PExpire(ctx, cur, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// Rates implements Store.
func (s *Redis) Rates(ctx context.Context, key string, fields ...string) ([]float64, error) {
	now := s.now()
	cur, prev := s.keys(key, now)
	pipe := s.rdb.Pipeline()
	curCmd := pipe.HMGet(ctx, cur, fields...)
	prevCmd := pipe.HMGet(ctx, prev, fields...)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	pw := prevWeight(now)
	out := make([]float64, len(fields))
	for i := range fields {
		out[i] = toFloat(prevCmd.Val()[i])*pw + toFloat(curCmd.Val()[i])
	}
	return out, nil
}

// takeScript checks and increments atomically so concurrent replicas cannot
// all slip under the limit at once.
var takeScript = redis.NewScript(`
local cur  = tonumber(redis.call('HGET', KEYS[1], 'n') or '0')
local prev = tonumber(redis.call('HGET', KEYS[2], 'n') or '0')
if prev * tonumber(ARGV[1]) + cur + 1 > tonumber(ARGV[2]) then
  return {0, tostring(prev), tostring(cur)}
end
redis.call('HINCRBY', KEYS[1], 'n', 1)
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return {1, tostring(prev), tostring(cur)}
`)

// Take implements Store.
func (s *Redis) Take(ctx context.Context, key string, limit float64) (bool, time.Duration, error) {
	now := s.now()
	cur, prev := s.keys(key, now)
	res, err := takeScript.Run(ctx, s.rdb, []string{cur, prev},
		prevWeight(now), limit, ttl.Milliseconds()).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(res) != 3 {
		return false, 0, fmt.Errorf("state: unexpected script result %v", res)
	}
	if ok, _ := res[0].(int64); ok == 1 {
		return true, 0, nil
	}
	return false, retryAfter(now, toFloat(res[1]), toFloat(res[2]), limit), nil
}

// Close implements Store.
func (s *Redis) Close() error { return s.rdb.Close() }

func toFloat(v any) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case int64:
		return float64(x)
	case float64:
		return x
	default:
		return 0
	}
}
