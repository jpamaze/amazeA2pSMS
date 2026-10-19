// Package config loads service configuration from environment variables.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Common holds settings shared by every service.
type Common struct {
	ServiceName string `env:"SERVICE_NAME"`
	Env         string `env:"APP_ENV" envDefault:"dev"`
	LogLevel    string `env:"LOG_LEVEL" envDefault:"info"`
	OpsAddr     string `env:"OPS_ADDR" envDefault:":9090"` // /healthz, /readyz, /metrics

	PostgresURL  string   `env:"POSTGRES_URL" envDefault:"postgres://sms:sms@localhost:5432/sms?sslmode=disable"`
	RedisAddrs   []string `env:"REDIS_ADDRS" envSeparator:"," envDefault:"localhost:6379"`
	KafkaBrokers []string `env:"KAFKA_BROKERS" envSeparator:"," envDefault:"localhost:9092"`
}

// Load parses environment variables into cfg, which must be a pointer to a struct.
func Load(cfg any) error {
	if err := env.Parse(cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
