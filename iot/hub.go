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

// Subscribe returns pings for one vehicle (local + Redis).
func (h *Hub) Subscribe(vehicleID string) (<-chan Ping, func()) {
	if h == nil {
		ch := make(chan Ping)
		close(ch)
		return ch, func() {}
	}
	out := make(chan Ping, 32)
	localCh, localCancel := h.local.Subscribe(vehicleID)

	var (
		redisCancel context.CancelFunc
		wg          sync.WaitGroup
	)

	forward := func(p Ping) {
		select {
		case out <- p:
		default:
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range localCh {
			forward(p)
		}
	}()

	if h.redis != nil {
		var ctx context.Context
		ctx, redisCancel = context.WithCancel(context.Background())
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := h.redis.Subscribe(ctx, redisVehicleChannelPrefix+vehicleID)
			defer func() { _ = sub.Close() }()
			ch := sub.Channel()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}
					var p Ping
					if json.Unmarshal([]byte(msg.Payload), &p) == nil {
						forward(p)
					}
				}
			}
		}()
	}

	cancel := func() {
		localCancel()
		if redisCancel != nil {
			redisCancel()
		}
		wg.Wait()
		close(out)
	}
	return out, cancel
}

// SubscribeLive receives any vehicle ping published to the fleet live channel.
func (h *Hub) SubscribeLive() (<-chan Ping, func()) {
	if h == nil || h.redis == nil {
		ch := make(chan Ping)
		close(ch)
		return ch, func() {}
	}
	out := make(chan Ping, 64)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sub := h.redis.Subscribe(ctx, redisLiveChannel)
		defer func() { _ = sub.Close() }()
		for msg := range sub.Channel() {
			var p Ping
			if json.Unmarshal([]byte(msg.Payload), &p) == nil && p.VehicleID != "" {
				select {
				case out <- p:
				default:
				}
			}
		}
	}()
	return out, func() {
		cancel()
		wg.Wait()
		close(out)
	}
}
