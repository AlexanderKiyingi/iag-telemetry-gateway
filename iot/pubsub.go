package iot

import "sync"

// Broker is an in-memory fan-out for live telemetry. Publishers (the
// ingestion paths — both HTTP and the TCP gateway) call Publish; SSE
// handlers call Subscribe to get a channel of pings for one vehicle.
//
// This is single-process by design. For a multi-process API tier, swap
// for a shared pubsub (Redis PUBLISH telemetry:<vehicleId>, NATS, etc.) —
// the public methods are the integration boundary.
type Broker struct {
	mu       sync.RWMutex
	subs     map[string]map[chan Ping]struct{} // vehicleID → set of subscribers
	liveSubs map[chan Ping]struct{}            // fleet-wide live map subscribers
}

func NewBroker() *Broker {
	return &Broker{
		subs:     make(map[string]map[chan Ping]struct{}),
		liveSubs: make(map[chan Ping]struct{}),
	}
}

// SubscribeLive receives every ping published on this process (all vehicles).
func (b *Broker) SubscribeLive() (<-chan Ping, func()) {
	ch := make(chan Ping, 64)
	b.mu.Lock()
	b.liveSubs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		delete(b.liveSubs, ch)
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// Subscribe returns a buffered channel that receives every Ping published
// for the given vehicleID. The caller MUST call the returned cancel to
// release the subscription when done — leaking subscribers leaks memory
// and slowly stalls Publish (sends are non-blocking, but the map grows).
func (b *Broker) Subscribe(vehicleID string) (<-chan Ping, func()) {
	ch := make(chan Ping, 16)

	b.mu.Lock()
	if _, ok := b.subs[vehicleID]; !ok {
		b.subs[vehicleID] = make(map[chan Ping]struct{})
	}
	b.subs[vehicleID][ch] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if set, ok := b.subs[vehicleID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, vehicleID)
			}
		}
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// Publish delivers a ping to every subscriber for that vehicle.
// Sends are non-blocking — if a subscriber's buffer is full we drop the
// ping for that subscriber rather than block ingest. SSE clients on a
// slow link will see gaps, never a stalled writer.
func (b *Broker) Publish(p Ping) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs[p.VehicleID] {
		select {
		case ch <- p:
		default:
		}
	}
	for ch := range b.liveSubs {
		select {
		case ch <- p:
		default:
		}
	}
}
