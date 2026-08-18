package iot

import (
	"errors"
	"testing"
	"time"
)

func speed(v float64) *float64 { return &v }

// The refusal paths are the feature. Each of these is a way someone could be
// hurt if the interlock let it through.
func TestEvaluateInterlock_immobilizeRefusals(t *testing.T) {
	cfg := DefaultInterlockConfig()
	fresh := 10 * time.Second

	cases := []struct {
		name     string
		snap     VehicleSnapshot
		queued   time.Duration
		wantWord string
	}{
		{
			name:     "moving at speed",
			snap:     VehicleSnapshot{SpeedKmh: speed(64), FixAge: fresh, HasFix: true},
			wantWord: "moving",
		},
		{
			name:     "just above the walking-pace limit",
			snap:     VehicleSnapshot{SpeedKmh: speed(5.1), FixAge: fresh, HasFix: true},
			wantWord: "moving",
		},
		{
			name:     "stale fix — position unknown",
			snap:     VehicleSnapshot{SpeedKmh: speed(0), FixAge: 10 * time.Minute, HasFix: true},
			wantWord: "old",
		},
		{
			name:     "no fix at all",
			snap:     VehicleSnapshot{SpeedKmh: speed(0), HasFix: false},
			wantWord: "blind",
		},
		{
			name:     "no speed reading",
			snap:     VehicleSnapshot{SpeedKmh: nil, FixAge: fresh, HasFix: true},
			wantWord: "speed",
		},
		{
			name:     "queued too long",
			snap:     VehicleSnapshot{SpeedKmh: speed(0), FixAge: fresh, HasFix: true},
			queued:   30 * time.Minute,
			wantWord: "expired",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := EvaluateInterlock(CommandImmobilize, tc.queued, tc.snap, cfg)
			if v.Allowed {
				t.Fatalf("immobilise must be refused; got allowed with reason %q", v.Reason)
			}
			if !contains(v.Reason, tc.wantWord) {
				t.Fatalf("reason %q should mention %q — it is shown to the operator and stored in the audit trail",
					v.Reason, tc.wantWord)
			}
		})
	}
}

func TestEvaluateInterlock_immobilizeAllowedWhenStationary(t *testing.T) {
	cfg := DefaultInterlockConfig()
	v := EvaluateInterlock(CommandImmobilize, time.Minute,
		VehicleSnapshot{SpeedKmh: speed(0), FixAge: 5 * time.Second, HasFix: true}, cfg)
	if !v.Allowed {
		t.Fatalf("a stationary vehicle with a fresh fix should be immobilisable; refused: %s", v.Reason)
	}
}

// Refusing to release an engine would strand a vehicle, which is its own
// safety problem — so mobilise is not gated on speed or fix.
func TestEvaluateInterlock_mobilizeAlwaysAllowed(t *testing.T) {
	cfg := DefaultInterlockConfig()
	for _, snap := range []VehicleSnapshot{
		{SpeedKmh: speed(90), FixAge: time.Hour, HasFix: true},
		{HasFix: false},
	} {
		if v := EvaluateInterlock(CommandMobilize, time.Minute, snap, cfg); !v.Allowed {
			t.Fatalf("mobilize must always be permitted; refused: %s", v.Reason)
		}
	}
}

// ...but an expired command is still expired, in either direction.
func TestEvaluateInterlock_expiryAppliesToMobilizeToo(t *testing.T) {
	cfg := DefaultInterlockConfig()
	v := EvaluateInterlock(CommandMobilize, 30*time.Minute,
		VehicleSnapshot{SpeedKmh: speed(0), HasFix: true}, cfg)
	if v.Allowed {
		t.Fatal("an expired command must not be delivered regardless of kind")
	}
}

func TestEvaluateInterlock_unknownKindRefused(t *testing.T) {
	v := EvaluateInterlock("self_destruct", 0,
		VehicleSnapshot{SpeedKmh: speed(0), HasFix: true}, DefaultInterlockConfig())
	if v.Allowed {
		t.Fatal("an unrecognised command kind must be refused")
	}
}

// No encoder is registered by default, so immobilisation is unavailable until
// someone confirms the framing against real hardware. A guessed byte sequence
// could mean something entirely different on a clone.
func TestEncodeCommand_refusesUnknownModel(t *testing.T) {
	if _, err := EncodeCommand("ST-999-NEVER-SEEN", CommandImmobilize); !errors.Is(err, ErrNoEncoder) {
		t.Fatalf("err = %v, want ErrNoEncoder", err)
	}
}

func TestEncodeCommand_usesRegisteredEncoder(t *testing.T) {
	RegisterCommandEncoder("ST-TEST", func(kind string) ([]byte, error) {
		if kind != CommandImmobilize {
			return nil, ErrUnknownKind
		}
		return []byte("*HQ,TEST,S20,1#"), nil
	})

	got, err := EncodeCommand("st-test", CommandImmobilize) // case-insensitive
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got) != "*HQ,TEST,S20,1#" {
		t.Fatalf("payload = %q", got)
	}

	if _, err := EncodeCommand("ST-TEST", CommandMobilize); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("an encoder that does not support a kind must say so, got %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
