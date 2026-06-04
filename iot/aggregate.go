package iot

import (
	"math"
	"sort"
	"time"
)

// movingThresholdKmh classifies pings as moving vs idle. Below this we
// assume GPS jitter / a stationary vehicle. 5 km/h is a common default in
// telematics products.
const movingThresholdKmh = 5.0

// gapCapMinutes caps how much wall time a single inter-ping interval
// contributes to moving/idle minutes. If a device drops off the network
// for hours, we don't want to retroactively call the vehicle "idle" for
// that whole gap — that's gateway loss, not real driving state.
const gapCapMinutes = 10

// Fuel-event detection thresholds. Tuned for the typical 1-ping/min
// cadence — if your fleet pings far less often, raise refuelMinPct to
// avoid flagging slow normal consumption as a "drop".
const (
	// Smallest positive fuel-level delta between consecutive pings that
	// counts as a refuel. 5% of a 200L tank ≈ 10L.
	refuelMinPct = 5.0

	// Smallest negative delta that triggers a drop event. Typical
	// consumption is < 1%/minute even at full throttle, so a 3% sudden
	// drop is well outside normal driving.
	dropMinPct = 3.0

	// Maximum gap between consecutive pings for the delta to be
	// attributable to one event. A 4-hour silence with the level having
	// fallen 4% just means the vehicle drove during the gap — not a drop.
	fuelEventMaxGapMinutes = 5.0
)

// AggregateDay reduces a day's pings (oldest first) into a DailyResult:
// the summary row plus any fuel events detected over the day. Pure
// function: no DB, easy to unit-test.
//
// Caller is responsible for:
//   - filtering pings to a single (vehicleID, day)
//   - passing the vehicle's tank capacity in litres (0 = unknown; fuel
//     metrics in litres are then nil and only % deltas are reported)
//
// Distance uses haversine on consecutive pings; this overestimates very
// slightly on long straight legs and underestimates on tight curves with
// sparse pings. For ~1ppm cadence both errors are sub-1%.
func AggregateDay(vehicleID string, day time.Time, pings []Ping, tankCapacityLitres int) DailyResult {
	sum, events := aggregateAndDetect(vehicleID, day, pings, tankCapacityLitres)
	return DailyResult{Summary: sum, FuelEvents: events}
}

// aggregateAndDetect is the inner implementation; split so distance/moving
// computation and fuel computation share a single pass over the ping list.
func aggregateAndDetect(vehicleID string, day time.Time, pings []Ping, tankCapacityLitres int) (DailySummary, []FuelEvent) {
	sum := DailySummary{
		VehicleID: vehicleID,
		Day:       day.UTC().Truncate(24 * time.Hour),
		PingCount: len(pings),
	}
	if len(pings) == 0 {
		return sum, nil
	}
	if !sort.SliceIsSorted(pings, func(i, j int) bool { return pings[i].TS.Before(pings[j].TS) }) {
		sort.Slice(pings, func(i, j int) bool { return pings[i].TS.Before(pings[j].TS) })
	}

	first := pings[0].TS
	last := pings[len(pings)-1].TS
	sum.FirstPing = &first
	sum.LastPing = &last

	var (
		distanceKm   float64
		maxSpeed     float64
		speedSum     float64
		speedSamples int
		movingMin    float64
		idleMin      float64

		// Fuel: sum of negative deltas between consecutive pings, EXCLUDING
		// jumps that trip the refuel detector. consumedPct accumulates as a
		// positive number; we negate to get "used".
		consumedPct      float64
		hasFuelReadings  bool
		events           []FuelEvent
	)

	for i, p := range pings {
		if p.SpeedKmh != nil {
			if *p.SpeedKmh > maxSpeed {
				maxSpeed = *p.SpeedKmh
			}
			speedSum += *p.SpeedKmh
			speedSamples++
		}
		if p.FuelLevel != nil {
			hasFuelReadings = true
		}

		if i == 0 {
			continue
		}
		prev := pings[i-1]
		distanceKm += HaversineKm(prev.Lat, prev.Lng, p.Lat, p.Lng)

		// Classify the gap [prev.TS, p.TS] using the average of the two
		// endpoint speeds (when both known). Otherwise fall back to whichever
		// endpoint reported a speed; if neither did, default to "idle".
		gapMin := p.TS.Sub(prev.TS).Minutes()
		if gapMin < 0 {
			continue
		}
		gap := gapMin
		if gap > gapCapMinutes {
			gap = gapCapMinutes
		}
		var sample float64
		switch {
		case prev.SpeedKmh != nil && p.SpeedKmh != nil:
			sample = (*prev.SpeedKmh + *p.SpeedKmh) / 2
		case p.SpeedKmh != nil:
			sample = *p.SpeedKmh
		case prev.SpeedKmh != nil:
			sample = *prev.SpeedKmh
		}
		if sample >= movingThresholdKmh {
			movingMin += gap
		} else {
			idleMin += gap
		}

		// Fuel-event detection + consumption tally.
		if prev.FuelLevel != nil && p.FuelLevel != nil {
			delta := *p.FuelLevel - *prev.FuelLevel

			switch {
			case delta >= refuelMinPct:
				events = append(events, buildFuelEvent("refuel", vehicleID, p, *prev.FuelLevel, *p.FuelLevel, delta, tankCapacityLitres, gapMin, sample))
				// Refuel jumps are excluded from "fuel used".
			case delta <= -dropMinPct && gapMin <= fuelEventMaxGapMinutes:
				events = append(events, buildFuelEvent("drop", vehicleID, p, *prev.FuelLevel, *p.FuelLevel, delta, tankCapacityLitres, gapMin, sample))
				consumedPct += -delta
			case delta < 0:
				// Normal consumption: add to the running tally.
				consumedPct += -delta
			}
		}
	}

	sum.DistanceKm = distanceKm
	if maxSpeed > 0 {
		sum.MaxSpeedKmh = &maxSpeed
	}
	if speedSamples > 0 {
		avg := speedSum / float64(speedSamples)
		sum.AvgSpeedKmh = &avg
	}
	sum.MovingMinutes = int(math.Round(movingMin))
	sum.IdleMinutes = int(math.Round(idleMin))

	if hasFuelReadings && tankCapacityLitres > 0 {
		used := consumedPct / 100.0 * float64(tankCapacityLitres)
		sum.FuelUsedLitres = &used
	}

	return sum, events
}

// buildFuelEvent assembles one FuelEvent. Confidence drops when the
// vehicle was moving fast (refuelling at speed is implausible — the
// device may have glitched) or when the gap between pings is large
// enough that the delta could be normal consumption rather than an event.
func buildFuelEvent(kind, vehicleID string, p Ping, before, after, delta float64, tankCapacityLitres int, gapMin, speedSample float64) FuelEvent {
	ev := FuelEvent{
		VehicleID:  vehicleID,
		Kind:       kind,
		TS:         p.TS,
		DeltaPct:   delta,
		BeforePct:  before,
		AfterPct:   after,
		Odo:        p.Odo,
		SpeedKmh:   p.SpeedKmh,
		Ignition:   p.Ignition,
		Confidence: "high",
	}
	if tankCapacityLitres > 0 {
		dl := delta / 100.0 * float64(tankCapacityLitres)
		ev.DeltaLitres = &dl
	}
	switch kind {
	case "refuel":
		// Refuelling while clearly moving is suspicious — likely a
		// telemetry glitch, not an actual fill.
		if speedSample >= movingThresholdKmh {
			ev.Confidence = "low"
			ev.Notes = "level rose while vehicle was moving"
		} else if p.Ignition != nil && *p.Ignition {
			ev.Confidence = "medium"
			ev.Notes = "level rose with ignition on"
		}
	case "drop":
		// A sharp drop while parked + ignition off is the textbook theft
		// pattern; if the vehicle was moving, the drop is more likely a
		// burst of normal consumption.
		switch {
		case p.Ignition != nil && !*p.Ignition && speedSample < movingThresholdKmh:
			ev.Confidence = "high"
			ev.Notes = "drop while parked, ignition off"
		case speedSample >= movingThresholdKmh:
			ev.Confidence = "low"
			ev.Notes = "drop while moving — possibly normal consumption"
		default:
			ev.Confidence = "medium"
		}
		if gapMin > fuelEventMaxGapMinutes/2 {
			ev.Confidence = "low"
		}
	}
	return ev
}
