// Package redisx builds the Redis client used for hot state.
package redisx

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// New returns a universal client: a single node for one address, a cluster
// client for several.
func New(addrs []string) redis.UniversalClient {
	return redis.NewUniversalClient(&redis.UniversalOptions{Addrs: addrs})
}

// Ping is a readiness check for a Redis client.
func Ping(c redis.UniversalClient) func(context.Context) error {
	return func(ctx context.Context) error { return c.Ping(ctx).Err() }
}
