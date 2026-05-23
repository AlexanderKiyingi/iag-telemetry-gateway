// Package iot owns telemetry ingestion + replay + live streaming.
//
// Pings come in two ways:
//   - HTTP bulk: POST /api/iot/pings, authenticated via the device's
//     shared API key (Authorization: Bearer <plaintext>). The plaintext
//     is hashed (sha256) on creation and only the digest is persisted.
//   - Codec 8/8E TCP: cmd/iot-gateway accepts native Teltonika
//     connections, identified by IMEI matching iot_devices.serial.
//
// Live updates fan out via an in-memory pubsub broker (one subscriber
// channel per active SSE client). For a multi-process deployment, swap
// the broker for Redis pubsub or NATS — the interface is the only thing
// callers depend on.
package iot

import (
	"encoding/json"
	"time"
)

type Device struct {
	ID         int64      `json:"id"`
	Serial     string     `json:"serial"`
	Label      string     `json:"label,omitempty"`
	VehicleID  string     `json:"vehicleId,omitempty"`
	HasAPIKey  bool       `json:"hasApiKey"`
	IsActive   bool       `json:"isActive"`
	LastSeen   *time.Time `json:"lastSeen,omitempty"`
	LastIP     string     `json:"lastIp,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// Ping is one position record, persisted to telemetry_timeseries (Timescale hypertable).
// Lat/Lng are required; everything else is best-effort and may be nil
// depending on the device firmware and configured IO map.
type Ping struct {
	ID         int64           `json:"id,omitempty"` // deprecated: not stored; use vehicleId + ts
	VehicleID  string          `json:"vehicleId"`
	DeviceID   *int64          `json:"deviceId,omitempty"`
	TS         time.Time       `json:"ts"`
	Lat        float64         `json:"lat"`
	Lng        float64         `json:"lng"`
	Altitude   *float64        `json:"altitude,omitempty"`
	Heading    *float64        `json:"heading,omitempty"`
	SpeedKmh   *float64        `json:"speedKmh,omitempty"`
	Satellites *int            `json:"satellites,omitempty"`
	Odo        *float64        `json:"odo,omitempty"`
	FuelLevel  *float64        `json:"fuelLevel,omitempty"`
	Ignition   *bool           `json:"ignition,omitempty"`
	EventID    *int            `json:"eventId,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// FuelEvent is an auto-detected refuel or anomalous drop, persisted to
// fuel_events. delta_pct is positive for refuels, negative for drops;
// delta_litres is set only when the vehicle's tank_capacity_litres is known.
type FuelEvent struct {
	ID          int64     `json:"id,omitempty"`
	VehicleID   string    `json:"vehicleId"`
	Kind        string    `json:"kind"` // refuel | drop
	TS          time.Time `json:"ts"`
	DeltaPct    float64   `json:"deltaPct"`
	DeltaLitres *float64  `json:"deltaLitres,omitempty"`
	BeforePct   float64   `json:"beforePct"`
	AfterPct    float64   `json:"afterPct"`
	Odo         *float64  `json:"odo,omitempty"`
	SpeedKmh    *float64  `json:"speedKmh,omitempty"`
	Ignition    *bool     `json:"ignition,omitempty"`
	Confidence    string    `json:"confidence"` // high | medium | low
	Notes         string    `json:"notes,omitempty"`
	FuelRecordID  string    `json:"fuelRecordId,omitempty"`
}

// DailyResult bundles what AggregateDay produces: the summary row plus any
// fuel events detected over the day's pings.
type DailyResult struct {
	Summary    DailySummary `json:"summary"`
	FuelEvents []FuelEvent  `json:"fuelEvents,omitempty"`
}

// DailySummary is one row of telemetry_daily.
type DailySummary struct {
	VehicleID       string    `json:"vehicleId"`
	Day             time.Time `json:"day"`
	PingCount       int       `json:"pingCount"`
	DistanceKm      float64   `json:"distanceKm"`
	MaxSpeedKmh     *float64  `json:"maxSpeedKmh,omitempty"`
	AvgSpeedKmh     *float64  `json:"avgSpeedKmh,omitempty"`
	FuelUsedLitres  *float64  `json:"fuelUsedLitres,omitempty"`
	MovingMinutes   int       `json:"movingMinutes"`
	IdleMinutes     int       `json:"idleMinutes"`
	FirstPing       *time.Time `json:"firstPing,omitempty"`
	LastPing        *time.Time `json:"lastPing,omitempty"`
}
