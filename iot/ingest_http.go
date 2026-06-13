package iot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

const MaxIngestBatch = 1000

// IngestPingBody is one HTTP ingest row (device relay or manual POST).
type IngestPingBody struct {
	VehicleID  string          `json:"vehicleId"`
	TS         time.Time       `json:"ts"`
	Lat        float64         `json:"lat"`
	Lng        float64         `json:"lng"`
	Altitude   *float64        `json:"altitude"`
	Heading    *float64        `json:"heading"`
	SpeedKmh   *float64        `json:"speedKmh"`
	Satellites *int            `json:"satellites"`
	Odo        *float64        `json:"odo"`
	FuelLevel  *float64        `json:"fuelLevel"`
	Ignition   *bool           `json:"ignition"`
	EventID    *int            `json:"eventId"`
	Raw        json.RawMessage `json:"raw"`
}

// IngestBatchResult is the HTTP ingest outcome. Pings are persisted even when
// registry sync fails; callers can inspect RegistrySync* for partial failures.
type IngestBatchResult struct {
	Accepted            int    `json:"accepted"`
	RegistrySyncFailed  bool   `json:"registrySyncFailed,omitempty"`
	RegistrySyncError   string `json:"registrySyncError,omitempty"`
}

// IngestHTTPBatch authenticates via device API key, validates, persists pings,
// syncs vehicle hot-state, and publishes to the telemetry hub.
func IngestHTTPBatch(ctx context.Context, store *Store, hub *Hub, apiKey string, body []byte, clientIP string) (IngestBatchResult, error) {
	device, err := store.AuthenticateAPIKey(ctx, apiKey)
	if err != nil {
		return IngestBatchResult{}, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) > 0 && body[0] == '{' {
		body = []byte("[" + string(body) + "]")
	}
	var batch []IngestPingBody
	if err := json.Unmarshal(body, &batch); err != nil {
		return IngestBatchResult{}, fmt.Errorf("malformed body: %w", err)
	}
	if len(batch) == 0 {
		return IngestBatchResult{}, fmt.Errorf("empty batch")
	}
	if len(batch) > MaxIngestBatch {
		return IngestBatchResult{}, fmt.Errorf("batch exceeds %d pings", MaxIngestBatch)
	}
	now := time.Now().UTC()
	pings := make([]Ping, 0, len(batch))
	for _, b := range batch {
		// A device bound to a vehicle may only post telemetry for that vehicle.
		// Without this, any valid API key could inject pings for arbitrary
		// vehicleIds it does not own. Unbound devices (no default binding) must
		// name a vehicleId explicitly.
		vehicleID := device.VehicleID
		if device.VehicleID != "" {
			if b.VehicleID != "" && b.VehicleID != device.VehicleID {
				return IngestBatchResult{}, fmt.Errorf("vehicleId %q does not match device binding", b.VehicleID)
			}
		} else {
			vehicleID = b.VehicleID
			if vehicleID == "" {
				return IngestBatchResult{}, fmt.Errorf("vehicleId required (device has no default binding)")
			}
		}
		if b.Lat < -90 || b.Lat > 90 || b.Lng < -180 || b.Lng > 180 {
			return IngestBatchResult{}, fmt.Errorf("invalid coordinates")
		}
		ts := b.TS
		if ts.IsZero() {
			ts = now
		} else {
			if ts.Before(now.Add(-24 * time.Hour)) {
				return IngestBatchResult{}, fmt.Errorf("timestamp too old")
			}
			if ts.After(now.Add(5 * time.Minute)) {
				return IngestBatchResult{}, fmt.Errorf("timestamp in the future")
			}
		}
		raw := b.Raw
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		devID := device.ID
		pings = append(pings, Ping{
			VehicleID: vehicleID, DeviceID: &devID, TS: ts,
			Lat: b.Lat, Lng: b.Lng, Altitude: b.Altitude, Heading: b.Heading,
			SpeedKmh: b.SpeedKmh, Satellites: b.Satellites, Odo: b.Odo,
			FuelLevel: b.FuelLevel, Ignition: b.Ignition, EventID: b.EventID, Raw: raw,
		})
	}
	n, err := store.InsertPings(ctx, pings)
	if err != nil {
		return IngestBatchResult{}, err
	}
	result := IngestBatchResult{Accepted: n}
	if newest := newestPing(pings); newest != nil && newest.VehicleID != "" {
		if _, err := store.ApplyVehicleHotState(ctx, *newest); err != nil {
			slog.Warn("registry sync failed after ingest",
				"vehicleId", newest.VehicleID, "err", err)
			result.RegistrySyncFailed = true
			result.RegistrySyncError = err.Error()
		}
		if err := store.ApplyGeofenceTransitions(ctx, ProcessGeofences(*newest)); err != nil {
			slog.Warn("geofence transitions failed after ingest",
				"vehicleId", newest.VehicleID, "err", err)
		}
	}
	if err := store.MarkSeen(ctx, device.ID, clientIP); err != nil {
		slog.Warn("mark device seen failed", "deviceId", device.ID, "err", err)
	}
	if hub != nil {
		for _, p := range pings {
			hub.Publish(p)
		}
	}
	return result, nil
}

// ReadIngestBody reads and normalizes a request body for IngestHTTPBatch.
func ReadIngestBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func newestPing(pings []Ping) *Ping {
	if len(pings) == 0 {
		return nil
	}
	best := pings[0]
	for _, p := range pings[1:] {
		if p.TS.After(best.TS) {
			best = p
		}
	}
	return &best
}
