package iot

import (
	"testing"
	"time"
)

// The rule is pure, so every branch of the sustain/hysteresis logic is testable
// without a database. These cases are the difference between an alert people
// act on and one they filter to a folder.

func at(sec int) time.Time {
	return time.Date(2026, 8, 18, 9, 0, sec, 0, time.UTC)
}

const testLimit = 80.0

func TestEvaluateOverspeed_singleSpikeDoesNotAlert(t *testing.T) {
	// One bad GPS sample at 95 opens a breach but must not alert: it has not
	// been sustained. This is the most common false positive on cheap trackers.
	d := evaluateOverspeed(overspeedState{}, 95, testLimit, at(0), 30*time.Second)
	if d.Alert {
		t.Fatal("a single sample over the limit must not alert")
	}
	if !d.Next.breaching {
		t.Fatal("expected the breach to be open and pending")
	}
	if d.Next.alerted {
		t.Fatal("breach should not be marked alerted yet")
	}
}

func TestEvaluateOverspeed_sustainedBreachAlertsOnce(t *testing.T) {
	st := overspeedState{}

	// t=0 opens the breach.
	d := evaluateOverspeed(st, 90, testLimit, at(0), 30*time.Second)
	st = d.Next
	if d.Alert {
		t.Fatal("must not alert on the opening sample")
	}

	// t=15 still over but not yet sustained.
	d = evaluateOverspeed(st, 92, testLimit, at(15), 30*time.Second)
	st = d.Next
	if d.Alert {
		t.Fatal("15s is under the 30s sustain threshold")
	}

	// t=30 crosses the threshold — this is the alert.
	d = evaluateOverspeed(st, 97, testLimit, at(30), 30*time.Second)
	st = d.Next
	if !d.Alert {
		t.Fatal("expected an alert once the breach was sustained")
	}
	if st.peakKmh != 97 {
		t.Fatalf("peak = %v, want 97", st.peakKmh)
	}

	// t=45, t=60 — still speeding, but the event was already raised. A long
	// highway run is one event, not one per ping.
	for _, s := range []int{45, 60} {
		d = evaluateOverspeed(st, 99, testLimit, at(s), 30*time.Second)
		st = d.Next
		if d.Alert {
			t.Fatalf("re-alerted at t=%d during the same breach", s)
		}
	}
	if st.peakKmh != 99 {
		t.Fatalf("peak = %v, want it to track the maximum (99)", st.peakKmh)
	}
}

func TestEvaluateOverspeed_hysteresisPreventsFlapping(t *testing.T) {
	// An alerted breach, vehicle now sitting just under the limit.
	st := overspeedState{breaching: true, startedAt: at(0), peakKmh: 95, alerted: true}

	// 78 is under the limit but inside the 5 km/h margin — hold state.
	d := evaluateOverspeed(st, 78, testLimit, at(60), 30*time.Second)
	if !d.Next.breaching {
		t.Fatal("inside the hysteresis band the breach must be held, not closed")
	}
	if d.Alert {
		t.Fatal("no alert while holding")
	}

	// Back over without having cleared: must not raise a second event.
	d = evaluateOverspeed(d.Next, 88, testLimit, at(75), 30*time.Second)
	if d.Alert {
		t.Fatal("bouncing around the limit must not produce a second event")
	}

	// 70 is below limit − margin: the breach closes and re-arms.
	d = evaluateOverspeed(d.Next, 70, testLimit, at(90), 30*time.Second)
	if d.Next.breaching || d.Next.alerted {
		t.Fatalf("expected the breach to close and re-arm, got %+v", d.Next)
	}
}

func TestEvaluateOverspeed_newBreachAfterClearAlertsAgain(t *testing.T) {
	// A genuinely separate speeding incident should alert again.
	st := overspeedState{} // cleared

	d := evaluateOverspeed(st, 90, testLimit, at(200), 30*time.Second)
	st = d.Next
	d = evaluateOverspeed(st, 91, testLimit, at(240), 30*time.Second)
	if !d.Alert {
		t.Fatal("a new sustained breach after clearing must alert")
	}
}

func TestEvaluateOverspeed_ignoresGarbageAndDisabled(t *testing.T) {
	cases := []struct {
		name  string
		speed float64
		limit float64
	}{
		{"no limit configured", 120, 0},
		{"negative limit", 120, -1},
		{"zero speed", 0, testLimit},
		{"implausible GPS spike", 450, testLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := evaluateOverspeed(overspeedState{}, tc.speed, tc.limit, at(0), 30*time.Second)
			if d.Alert {
				t.Fatal("must not alert")
			}
			if d.Next.breaching {
				t.Fatal("must not open a breach")
			}
		})
	}
}

func TestEvaluateOverspeed_zeroSustainAlertsImmediately(t *testing.T) {
	// Opting into instant alerting is legitimate (a yard with a hard 20 km/h
	// rule), but it has to be explicit rather than the default.
	d := evaluateOverspeed(overspeedState{}, 90, testLimit, at(0), 0)
	if !d.Alert {
		t.Fatal("minDur=0 means alert on the first sample over")
	}
	if !d.Next.alerted {
		t.Fatal("state must record that this breach already alerted")
	}
}

func TestOverspeedConfigFromEnv(t *testing.T) {
	t.Run("absent leaves monitoring off", func(t *testing.T) {
		t.Setenv("FLEET_SPEED_LIMIT_KMH", "")
		cfg := OverspeedConfigFromEnv()
		if cfg.Enabled() {
			t.Fatal("monitoring must be opt-in")
		}
	})
	t.Run("parses a limit", func(t *testing.T) {
		t.Setenv("FLEET_SPEED_LIMIT_KMH", "80")
		t.Setenv("FLEET_OVERSPEED_MIN_SECONDS", "45")
		cfg := OverspeedConfigFromEnv()
		if cfg.DefaultLimitKmh != 80 {
			t.Fatalf("limit = %v, want 80", cfg.DefaultLimitKmh)
		}
		if cfg.MinBreachDuration != 45*time.Second {
			t.Fatalf("sustain = %v, want 45s", cfg.MinBreachDuration)
		}
	})
	t.Run("garbage does not enable monitoring", func(t *testing.T) {
		t.Setenv("FLEET_SPEED_LIMIT_KMH", "fast")
		if OverspeedConfigFromEnv().Enabled() {
			t.Fatal("unparseable limit must leave monitoring off")
		}
	})
}
