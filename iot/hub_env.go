package iot

import (
	"log/slog"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// NewHubFromEnv builds a telemetry hub with optional Redis pub/sub when
// REDIS_URL is set. Used by the API and iot-gateway processes.
func NewHubFromEnv() *Hub {
	local := NewBroker()
	redisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if redisURL == "" {
		return NewHub(local, nil)
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Warn("REDIS_URL invalid for telemetry hub; using in-process only", "err", err)
		return NewHub(local, nil)
	}
	client := redis.NewClient(opt)
	return NewHub(local, client)
}
