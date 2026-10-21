package msgstore

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bind registry: enforces max binds per system_id across all SMPP server
// nodes. Each live bind is a member of a sorted set scored by its lease
// expiry; nodes refresh their leases, so a crashed node's binds free up
// after one lease period.

func bindsKey(systemID string) string { return "binds:{" + systemID + "}" }

var acquireBind = redis.NewScript(`
local now = tonumber(ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZSCORE', KEYS[1], ARGV[2]) then
  return 1
end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[4]), ARGV[2])
redis.call('PEXPIRE', KEYS[1], 2 * tonumber(ARGV[4]))
return 1
`)

// AcquireBind registers member (a unique session ID) for systemID if fewer
// than max live binds exist. It returns false when the limit is reached.
func (s *Store) AcquireBind(ctx context.Context, systemID, member string, max int, lease time.Duration) (bool, error) {
	n, err := acquireBind.Run(ctx, s.rdb, []string{bindsKey(systemID)},
		time.Now().UnixMilli(), member, max, lease.Milliseconds()).Int()
	return n == 1, err
}

// RefreshBinds extends the leases of live binds.
func (s *Store) RefreshBinds(ctx context.Context, bySystem map[string][]string, lease time.Duration) error {
	exp := float64(time.Now().Add(lease).UnixMilli())
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for sys, members := range bySystem {
			zs := make([]redis.Z, len(members))
			for i, m := range members {
				zs[i] = redis.Z{Score: exp, Member: m}
			}
			p.ZAddXX(ctx, bindsKey(sys), zs...)
			p.PExpire(ctx, bindsKey(sys), 2*lease)
		}
		return nil
	})
	return err
}

// ReleaseBind frees a bind slot.
func (s *Store) ReleaseBind(ctx context.Context, systemID, member string) error {
	return s.rdb.ZRem(ctx, bindsKey(systemID), member).Err()
}
