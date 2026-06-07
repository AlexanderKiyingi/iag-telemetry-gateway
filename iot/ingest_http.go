package iot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// IngestHTTPBatch authenticates via device API key, validates, persists pings,
// syncs vehicle hot-state, and publishes to the telemetry hub.
func IngestHTTPBatch(ctx context.Context, store *Store, hub *Hub, apiKey string, body []byte, clientIP string) (accepted int, err error) {
	device, err := store.AuthenticateAPIKey(ctx, apiKey)
	if err != nil {
		return 0, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) > 0 && body[0] == '{' {
		body = []byte("[" + string(body) + "]")
	}
	var batch []IngestPingBody
	if err := json.Unmarshal(body, &batch); err != nil {
		return 0, fmt.Errorf("malformed body: %w", err)
	}
	if len(batch) == 0 {
		return 0, fmt.Errorf("empty batch")
	}
	if len(batch) > MaxIngestBatch {
		return 0, fmt.Errorf("batch exceeds %d pings", MaxIngestBatch)
	}
	now := time.Now().UTC()
	pings := make([]Ping, 0, len(batch))
	for _, b := range batch {
		vehicleID := b.VehicleID
		if vehicleID == "" {
			vehicleID = device.VehicleID
		}
		if vehicleID == "" {
			return 0, fmt.Errorf("vehicleId required (device has no default binding)")
		}
		if b.Lat < -90 || b.Lat > 90 || b.Lng < -180 || b.Lng > 180 {
			return 0, fmt.Errorf("invalid coordinates")
		}
		ts := b.TS
		if ts.IsZero() {
			ts = now
		} else {
			if ts.Before(now.Add(-24 * time.Hour)) {
				return 0, fmt.Errorf("timestamp too old")
			}
			if ts.After(now.Add(5 * time.Minute)) {
				return 0, fmt.Errorf("timestamp in the future")
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
		return 0, err
	}
	if newest := newestPing(pings); newest != nil && newest.VehicleID != "" {
		if syncRes, err := store.SyncVehicleFromPing(ctx, *newest); err == nil {
			_ = store.PublishStatusChange(ctx, syncRes)
		}
		_ = store.ApplyGeofenceTransitions(ctx, ProcessGeofences(*newest))
	}
	_ = store.MarkSeen(ctx, device.ID, clientIP)
	if hub != nil {
		for _, p := range pings {
			hub.Publish(p)
		}
	}
	return n, nil
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
