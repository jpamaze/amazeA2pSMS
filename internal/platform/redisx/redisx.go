// Package redisx builds the Redis client used for hot state.
package redisx

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Config says how to reach Redis.
type Config struct {
	// Addrs are the node addresses, or the Sentinel addresses when
	// SentinelMaster is set.
	Addrs []string
	// SentinelMaster is the master name the Sentinels watch. Set, the client
	// asks the Sentinels for the current primary and follows failovers.
	SentinelMaster   string
	Password         string
	SentinelPassword string
}

// New returns a universal client: a Sentinel-managed primary when
// SentinelMaster is set, else a single node for one address and a cluster
// client for several.
func New(c Config) redis.UniversalClient {
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:            c.Addrs,
		MasterName:       c.SentinelMaster,
		Password:         c.Password,
		SentinelPassword: c.SentinelPassword,
	})
}

// Ping is a readiness check for a Redis client.
func Ping(c redis.UniversalClient) func(context.Context) error {
	return func(ctx context.Context) error { return c.Ping(ctx).Err() }
}
