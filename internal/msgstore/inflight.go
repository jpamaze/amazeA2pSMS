package msgstore

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// inflightShards spreads the submitted-but-not-final index over several keys
// so no single Redis Cluster slot gets all of it.
const inflightShards = 16

func inflightKey(shard int) string { return fmt.Sprintf("inflight:%d", shard) }

func shardOf(id string) int {
	h := fnv.New32a()
	h.Write([]byte(id))
	return int(h.Sum32() % inflightShards)
}

// TrackInflight records that a message was submitted at t and is waiting for a DLR.
func (s *Store) TrackInflight(ctx context.Context, id string, t time.Time) error {
	return s.rdb.ZAdd(ctx, inflightKey(shardOf(id)), redis.Z{Score: float64(t.UnixMilli()), Member: id}).Err()
}

// UntrackInflight removes a message that reached a final status.
func (s *Store) UntrackInflight(ctx context.Context, id string) error {
	return s.rdb.ZRem(ctx, inflightKey(shardOf(id)), id).Err()
}

// Overdue returns up to limit messages submitted before cutoff that still
// have no final status.
func (s *Store) Overdue(ctx context.Context, cutoff time.Time, limit int64) ([]string, error) {
	var out []string
	max := strconv.FormatInt(cutoff.UnixMilli(), 10)
	for shard := range inflightShards {
		ids, err := s.rdb.ZRangeByScore(ctx, inflightKey(shard), &redis.ZRangeBy{
			Min: "-inf", Max: max, Count: limit - int64(len(out)),
		}).Result()
		if err != nil {
			return out, err
		}
		out = append(out, ids...)
		if int64(len(out)) >= limit {
			break
		}
	}
	return out, nil
}
