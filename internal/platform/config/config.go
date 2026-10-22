// Package config loads service configuration from environment variables.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/redisx"
)

// Common holds settings shared by every service.
type Common struct {
	ServiceName string `env:"SERVICE_NAME"`
	Env         string `env:"APP_ENV" envDefault:"production"` // dev-only behaviour needs APP_ENV=dev explicitly
	LogLevel    string `env:"LOG_LEVEL" envDefault:"info"`
	OpsAddr     string `env:"OPS_ADDR" envDefault:":9090"` // /healthz, /readyz, /metrics

	PostgresURL  string   `env:"POSTGRES_URL" envDefault:"postgres://sms:sms@localhost:5432/sms?sslmode=disable"`
	RedisAddrs   []string `env:"REDIS_ADDRS" envSeparator:"," envDefault:"localhost:6379"`
	KafkaBrokers []string `env:"KAFKA_BROKERS" envSeparator:"," envDefault:"localhost:9092"`

	// With Sentinel (production HA), REDIS_ADDRS lists the Sentinels and
	// REDIS_SENTINEL_MASTER names the primary they watch.
	RedisSentinelMaster   string `env:"REDIS_SENTINEL_MASTER"`
	RedisPassword         string `env:"REDIS_PASSWORD"`
	RedisSentinelPassword string `env:"REDIS_SENTINEL_PASSWORD"`
}

// Redis returns the Redis client settings.
func (c Common) Redis() redisx.Config {
	return redisx.Config{Addrs: c.RedisAddrs, SentinelMaster: c.RedisSentinelMaster,
		Password: c.RedisPassword, SentinelPassword: c.RedisSentinelPassword}
}

// Load parses environment variables into cfg, which must be a pointer to a struct.
func Load(cfg any) error {
	if err := env.Parse(cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
