package iot

import (
	"os"
	"strconv"
	"time"
)

// MaxTrackReplayRange is the maximum window for GET /api/vehicles/:id/track.
// Override with TELEMETRY_TRACK_MAX_DAYS (default 30).
func MaxTrackReplayRange() time.Duration {
	return time.Duration(envInt("TELEMETRY_TRACK_MAX_DAYS", 30)) * 24 * time.Hour
}

// MaxTrackRowLimit caps rows returned per track query.
func MaxTrackRowLimit() int {
	return envInt("TELEMETRY_TRACK_MAX_ROWS", 50000)
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
