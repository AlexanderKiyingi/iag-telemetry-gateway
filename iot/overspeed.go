package iot

// Server-side overspeed detection.
//
// Speed is on every ping regardless of protocol, so this works identically for
// HQ (SinoTrack), Teltonika and HTTP-relayed devices — unlike the tracker's own
// overspeed alarm, which is configured once per unit over SMS, is invisible to
// the platform, and on HQ hardware is buried in the undecoded status word.
//
// The hard part is not the comparison, it is not crying wolf. Three guards:
//
//   - SUSTAINED: GPS speed is noisy and a single sample can overshoot by
//     10 km/h on a bad fix. A breach must persist before it alerts.
//   - ALERT ONCE: a breach is a state with a beginning and an end. An hour on
//     the highway is one event, not two hundred.
//   - HYSTERESIS: re-arming only below limit − margin stops a vehicle sitting
//     on the limit from oscillating open/closed.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// defaultMinBreachDuration is how long a vehicle must stay above the limit
	// before it counts. Moving-interval on these trackers is ~15 s, so this is
	// comfortably more than one sample but still catches a short burst.
	defaultMinBreachDuration = 30 * time.Second

	// overspeedHysteresisKmh is how far below the limit the vehicle must fall
	// before a new breach can open.
	overspeedHysteresisKmh = 5.0

	// implausibleSpeedKmh discards obvious GPS garbage. Nothing in this fleet
	// does 300 km/h; a sample that claims to is a bad fix, and alerting on it
	// teaches people to ignore the alerts.
	implausibleSpeedKmh = 300.0
)

// OverspeedConfig resolves the limit for a vehicle.
type OverspeedConfig struct {
	// DefaultLimitKmh applies to vehicles with no explicit limit. Zero disables
	// monitoring entirely, which is the default — speeding alerts on a fleet
	// that never asked for them are worse than none.
	DefaultLimitKmh float64
	// MinBreachDuration overrides defaultMinBreachDuration when non-zero.
	MinBreachDuration time.Duration
}

// OverspeedConfigFromEnv reads FLEET_SPEED_LIMIT_KMH and
// FLEET_OVERSPEED_MIN_SECONDS. Absent or unparseable values leave monitoring off.
func OverspeedConfigFromEnv() OverspeedConfig {
	cfg := OverspeedConfig{MinBreachDuration: defaultMinBreachDuration}
	if v := os.Getenv("FLEET_SPEED_LIMIT_KMH"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.DefaultLimitKmh = f
		}
	}
	if v := os.Getenv("FLEET_OVERSPEED_MIN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MinBreachDuration = time.Duration(n) * time.Second
		}
	}
	return cfg
}

// Enabled reports whether any monitoring can happen at all.
func (c OverspeedConfig) Enabled() bool { return c.DefaultLimitKmh > 0 }

// overspeedState is the persisted breach state for one vehicle.
type overspeedState struct {
	breaching bool
	startedAt time.Time
	peakKmh   float64
	alerted   bool
}

// OverspeedDecision is what evaluateOverspeed concluded for one ping. Split out
// from the persistence so the rule itself is unit-testable without Postgres.
type OverspeedDecision struct {
	Next  overspeedState
	Alert bool    // raise a safety event now
	Limit float64 // limit in force, for the event text
}

// evaluateOverspeed is the whole rule, as a pure function of (previous state,
// this ping, limit). Kept separate from the database so the hysteresis and
// sustain logic can be tested exhaustively.
func evaluateOverspeed(prev overspeedState, speed, limit float64, ts time.Time, minDur time.Duration) OverspeedDecision {
	d := OverspeedDecision{Next: prev, Limit: limit}
	if limit <= 0 || speed <= 0 || speed >= implausibleSpeedKmh {
		return d
	}

	switch {
	case speed > limit:
		if !prev.breaching {
			d.Next = overspeedState{breaching: true, startedAt: ts, peakKmh: speed}
			// A breach never alerts on its opening sample; it has to be
			// sustained. With minDur == 0 the caller has explicitly opted into
			// alerting immediately.
			if minDur == 0 {
				d.Next.alerted = true
				d.Alert = true
			}
			return d
		}
		d.Next.peakKmh = math.Max(prev.peakKmh, speed)
		if !prev.alerted && !prev.startedAt.IsZero() && ts.Sub(prev.startedAt) >= minDur {
			d.Next.alerted = true
			d.Alert = true
		}
		return d

	case speed <= limit-overspeedHysteresisKmh:
		// Clearly back under: close the breach and re-arm.
		if prev.breaching {
			d.Next = overspeedState{}
		}
		return d

	default:
		// Inside the hysteresis band (between limit−margin and limit): hold
		// whatever state we were in rather than flapping.
		return d
	}
}

// ApplyOverspeed evaluates one ping against the vehicle's limit and raises a
// safety event when a sustained breach is confirmed. Safe to call for every
// ping; a no-op when monitoring is disabled or the vehicle has no limit.
func (s *Store) ApplyOverspeed(ctx context.Context, p Ping, cfg OverspeedConfig) error {
	if p.VehicleID == "" || p.SpeedKmh == nil {
		return nil
	}
	// The speed limit and the breach state are two rows keyed by the same
	// vehicle id, so they are one round trip, not two. This runs on every ping
	// with a speed reading — which is nearly all of them.
	limit, prev, err := s.loadOverspeedContext(ctx, p.VehicleID, cfg.DefaultLimitKmh)
	if err != nil {
		return err
	}
	if limit <= 0 {
		return nil
	}
	minDur := cfg.MinBreachDuration
	if minDur == 0 && cfg.MinBreachDuration == 0 {
		minDur = defaultMinBreachDuration
	}
	d := evaluateOverspeed(prev, *p.SpeedKmh, limit, p.TS, minDur)

	if d.Next != prev {
		if err := s.saveOverspeedState(ctx, p.VehicleID, d.Next, p.TS); err != nil {
			return err
		}
	}
	if !d.Alert {
		return nil
	}
	return s.insertOverspeedSafetyEvent(ctx, p, d)
}

// loadOverspeedContext reads the applicable speed limit and the current breach
// state in one round trip.
//
// The limit prefers the vehicle's own value; a stored 0 disables monitoring for
// that vehicle even when a fleet default exists, and an unknown vehicle returns
// 0 so nothing is evaluated. A vehicle with no state row yet is not an error —
// it is the zero state, meaning "not currently breaching".
//
// The LEFT JOIN matters for that last case: a vehicle that has never breached
// has a row in `vehicles` and none in `vehicle_overspeed_state`, and an inner
// join would silently stop monitoring exactly the vehicles that have behaved.
func (s *Store) loadOverspeedContext(ctx context.Context, vehicleID string, fallback float64) (float64, overspeedState, error) {
	var limit *float64
	var st overspeedState
	var breaching, alerted *bool
	var started *time.Time
	var peak *float64

	err := s.op().QueryRow(ctx, `
		SELECT v.speed_limit_kmh,
		       o.breaching, o.breach_started_at, o.peak_speed_kmh, o.alerted
		  FROM vehicles v
		  LEFT JOIN vehicle_overspeed_state o ON o.vehicle_id = v.id
		 WHERE v.id = $1`, vehicleID,
	).Scan(&limit, &breaching, &started, &peak, &alerted)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, overspeedState{}, nil
	}
	if err != nil {
		return 0, overspeedState{}, err
	}

	if breaching != nil {
		st.breaching = *breaching
	}
	if alerted != nil {
		st.alerted = *alerted
	}
	if peak != nil {
		st.peakKmh = *peak
	}
	if started != nil {
		st.startedAt = *started
	}

	if limit != nil {
		return *limit, st, nil
	}
	return fallback, st, nil
}

func (s *Store) saveOverspeedState(ctx context.Context, vehicleID string, st overspeedState, ts time.Time) error {
	var started *time.Time
	if !st.startedAt.IsZero() {
		started = &st.startedAt
	}
	_, err := s.op().Exec(ctx, `
		INSERT INTO vehicle_overspeed_state
		       (vehicle_id, breaching, breach_started_at, peak_speed_kmh, alerted, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (vehicle_id) DO UPDATE SET
			breaching         = EXCLUDED.breaching,
			breach_started_at = EXCLUDED.breach_started_at,
			peak_speed_kmh    = EXCLUDED.peak_speed_kmh,
			alerted           = EXCLUDED.alerted,
			updated_at        = EXCLUDED.updated_at`,
		vehicleID, st.breaching, started, st.peakKmh, st.alerted, ts)
	return err
}

// insertOverspeedSafetyEvent writes the alert. The id is derived from the
// vehicle and the breach start, so a retried ingest of the same breach cannot
// create a duplicate.
func (s *Store) insertOverspeedSafetyEvent(ctx context.Context, p Ping, d OverspeedDecision) error {
	start := d.Next.startedAt
	if start.IsZero() {
		start = p.TS
	}
	id := fmt.Sprintf("SAF-SPD-%s-%d", p.VehicleID, start.Unix())

	// Severity scales with how far over: 10% over a highway limit is a
	// different conversation from 50% over.
	severity := "warn"
	if d.Limit > 0 && d.Next.peakKmh >= d.Limit*1.25 {
		severity = "crit"
	}
	desc := fmt.Sprintf("Vehicle %s exceeded %.0f km/h (peak %.0f km/h)",
		p.VehicleID, d.Limit, d.Next.peakKmh)
	loc := fmt.Sprintf("%.4f, %.4f", p.Lat, p.Lng)

	_, err := s.op().Exec(ctx, `
		INSERT INTO safety_events (
			id, vehicle_id, date, type, severity, status, location, description, reported_by, status_history
		) VALUES ($1, $2, $3, 'Driver behaviour', $4, 'open', $5, $6, 'Speed monitor', '[]'::jsonb)
		ON CONFLICT (id) DO NOTHING`,
		id, p.VehicleID, start, severity, loc, desc)
	return err
}
