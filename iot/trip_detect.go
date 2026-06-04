package iot

import (
	"fmt"
	"sort"
	"time"
)

const (
	tripEndGapMinutes    = 20
	tripMinDistanceKm    = 0.5
	tripMinDurationMin   = 5
	tripStartSpeedKmh    = movingThresholdKmh
)

// DetectedTrip is a movement segment inferred from consecutive pings.
type DetectedTrip struct {
	VehicleID     string
	StartedAt     time.Time
	EndedAt       time.Time
	StartLocation string
	EndLocation   string
	DistanceKm    float64
	DurationMin   float64
}

// DetectTripsFromPings splits sorted pings into trips when the vehicle moves and gaps stay small.
func DetectTripsFromPings(vehicleID string, pings []Ping) []DetectedTrip {
	if len(pings) < 2 {
		return nil
	}
	if !sort.SliceIsSorted(pings, func(i, j int) bool { return pings[i].TS.Before(pings[j].TS) }) {
		sort.Slice(pings, func(i, j int) bool { return pings[i].TS.Before(pings[j].TS) })
	}

	var out []DetectedTrip
	var (
		active   bool
		start    Ping
		last     Ping
		distKm   float64
	)

	speedAt := func(p Ping) float64 {
		if p.SpeedKmh != nil {
			return *p.SpeedKmh
		}
		return 0
	}

	closeTrip := func() {
		if !active {
			return
		}
		dur := last.TS.Sub(start.TS).Minutes()
		if distKm >= tripMinDistanceKm && dur >= tripMinDurationMin {
			out = append(out, DetectedTrip{
				VehicleID:     vehicleID,
				StartedAt:     start.TS,
				EndedAt:       last.TS,
				StartLocation: formatCoord(start.Lat, start.Lng),
				EndLocation:   formatCoord(last.Lat, last.Lng),
				DistanceKm:    distKm,
				DurationMin:   dur,
			})
		}
		active = false
		distKm = 0
	}

	for i, p := range pings {
		if i > 0 {
			gap := p.TS.Sub(pings[i-1].TS).Minutes()
			if gap > tripEndGapMinutes {
				closeTrip()
			}
		}
		sp := speedAt(p)
		if !active {
			if sp >= tripStartSpeedKmh {
				active = true
				start = p
				last = p
			}
			continue
		}
		distKm += HaversineKm(last.Lat, last.Lng, p.Lat, p.Lng)
		last = p
		if sp < tripStartSpeedKmh {
			closeTrip()
		}
	}
	closeTrip()
	return out
}

func formatCoord(lat, lng float64) string {
	return fmt.Sprintf("%.4f, %.4f", lat, lng)
}
