package iot

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

const (
	redisVehicleChannelPrefix = "fleet:telemetry:vehicle:"
	redisLiveChannel          = "fleet:telemetry:live"
)

// Hub fans out telemetry pings in-process and via Redis pub/sub when a
// Redis client is configured (REDIS_URL). Use this instead of Broker
// directly so HTTP ingest and the TCP gateway can share live SSE across
// API replicas.
type Hub struct {
	local *Broker
	redis *redis.Client
}

// NewHub wraps a local broker and optional Redis client. Pass redis=nil for
// single-process deployments.
func NewHub(local *Broker, redis *redis.Client) *Hub {
	if local == nil {
		local = NewBroker()
	}
	return &Hub{local: local, redis: redis}
}

// Publish delivers a ping to local subscribers and Redis channels.
func (h *Hub) Publish(p Ping) {
	if h == nil {
		return
	}
	h.local.Publish(p)
	if h.redis == nil || p.VehicleID == "" {
		return
	}
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	ctx := context.Background()
	if err := h.redis.Publish(ctx, redisVehicleChannelPrefix+p.VehicleID, b).Err(); err != nil {
		slog.Debug("redis telemetry publish", "vehicleId", p.VehicleID, "err", err)
	}
	if err := h.redis.Publish(ctx, redisLiveChannel, b).Err(); err != nil {
		slog.Debug("redis fleet live publish", "err", err)
	}
}

// Subscribe returns pings for one vehicle. Uses Redis when configured, else in-process broker.
func (h *Hub) Subscribe(vehicleID string) (<-chan Ping, func()) {
	if h == nil {
		ch := make(chan Ping)
		close(ch)
		return ch, func() {}
	}
	out := make(chan Ping, 32)
	forward := func(p Ping) {
		select {
		case out <- p:
		default:
		}
	}

	if h.redis != nil {
		ctx, redisCancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := h.redis.Subscribe(ctx, redisVehicleChannelPrefix+vehicleID)
			defer func() { _ = sub.Close() }()
			for msg := range sub.Channel() {
				var p Ping
				if json.Unmarshal([]byte(msg.Payload), &p) == nil {
					forward(p)
				}
			}
		}()
		return out, func() {
			redisCancel()
			wg.Wait()
			close(out)
		}
	}

	localCh, localCancel := h.local.Subscribe(vehicleID)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range localCh {
			forward(p)
		}
	}()
	return out, func() {
		localCancel()
		wg.Wait()
		close(out)
	}
}

// SubscribeLive receives any vehicle ping. With Redis, only the pub/sub channel
// is used (avoids duplicate delivery when ingest and API share a process). Without
// Redis, the in-process broker is used.
func (h *Hub) SubscribeLive() (<-chan Ping, func()) {
	if h == nil {
		ch := make(chan Ping)
		close(ch)
		return ch, func() {}
	}
	out := make(chan Ping, 64)
	forward := func(p Ping) {
		if p.VehicleID == "" {
			return
		}
		select {
		case out <- p:
		default:
		}
	}

	if h.redis != nil {
		ctx, redisCancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := h.redis.Subscribe(ctx, redisLiveChannel)
			defer func() { _ = sub.Close() }()
			for msg := range sub.Channel() {
				var p Ping
				if json.Unmarshal([]byte(msg.Payload), &p) == nil {
					forward(p)
				}
			}
		}()
		return out, func() {
			redisCancel()
			wg.Wait()
			close(out)
		}
	}

	localCh, localCancel := h.local.SubscribeLive()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range localCh {
			forward(p)
		}
	}()
	return out, func() {
		localCancel()
		wg.Wait()
		close(out)
	}
}
