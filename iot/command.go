package iot

// Server→device commands, and the interlocks that decide whether one may be
// delivered.
//
// This exists for remote immobilisation. The wire format is the smallest part;
// the reason this file is mostly refusals is that cutting the engine on a moving
// vehicle can kill someone. Two rules shape everything here:
//
//  1. The interlock runs at DELIVERY, against the vehicle's state right now.
//     A command can sit queued while the truck pulls onto a highway, so
//     validating at request time proves nothing.
//
//  2. An unknown device model can never be sent anything. Encoders are
//     registered per model and there is no default — SinoTrack relay command
//     framing varies across firmwares, and a byte sequence that immobilises one
//     unit may mean something else on another. Guessing here is not a bug, it is
//     an injury.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Command kinds.
const (
	CommandImmobilize = "immobilize"
	CommandMobilize   = "mobilize"
)

// Command statuses.
const (
	CommandPending = "pending"
	CommandSent    = "sent"
	CommandRefused = "refused"
	CommandExpired = "expired"
	CommandFailed  = "failed"
)

var (
	// ErrNoEncoder means no verified command framing is registered for the
	// device's model. This is a refusal, never a fallback.
	ErrNoEncoder = errors.New("command: no verified encoder for this device model")
	// ErrUnknownKind guards against a command kind an encoder does not support.
	ErrUnknownKind = errors.New("command: unsupported command kind for this model")
)

// CommandEncoder turns a command kind into the exact bytes for one device
// model. Registering one is an assertion that the framing has been confirmed
// against real hardware.
type CommandEncoder func(kind string) ([]byte, error)

var (
	encodersMu sync.RWMutex
	encoders   = map[string]CommandEncoder{}
)

// RegisterCommandEncoder installs the framing for a model, e.g. "ST-901".
//
// Intentionally empty at startup. Immobilisation stays unavailable until
// someone confirms the byte sequence against a real unit and registers it here;
// until then every attempt is refused with ErrNoEncoder, which is the correct
// behaviour for a feature that can strand a vehicle.
func RegisterCommandEncoder(model string, enc CommandEncoder) {
	if model == "" || enc == nil {
		return
	}
	encodersMu.Lock()
	encoders[normaliseModel(model)] = enc
	encodersMu.Unlock()
}

// EncodeCommand renders a command for a model, or refuses.
func EncodeCommand(model, kind string) ([]byte, error) {
	encodersMu.RLock()
	enc, ok := encoders[normaliseModel(model)]
	encodersMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w (model %q)", ErrNoEncoder, model)
	}
	return enc(kind)
}

// SupportedCommandModels lists models with a registered encoder, so an operator
// UI can show immobilisation as available only where it actually is.
func SupportedCommandModels() []string {
	encodersMu.RLock()
	out := make([]string, 0, len(encoders))
	for m := range encoders {
		out = append(out, m)
	}
	encodersMu.RUnlock()
	sort.Strings(out)
	return out
}

func normaliseModel(m string) string {
	return strings.ToUpper(strings.TrimSpace(m))
}

// ─────────────────────────────── Interlocks ───────────────────────────────

// InterlockConfig bounds when an immobilise may be delivered.
type InterlockConfig struct {
	// MaxSpeedKmh is the speed at or below which immobilising is permitted.
	// Walking pace, not "slow": a truck at 20 km/h that loses its engine also
	// loses power steering and brake assist.
	MaxSpeedKmh float64
	// MaxFixAge rejects decisions made on stale telemetry. A vehicle that last
	// reported ten minutes ago may be anywhere.
	MaxFixAge time.Duration
	// MaxQueueAge expires a command that never found its device.
	MaxQueueAge time.Duration
}

// DefaultInterlockConfig is deliberately conservative.
func DefaultInterlockConfig() InterlockConfig {
	return InterlockConfig{
		MaxSpeedKmh: 5.0,
		MaxFixAge:   2 * time.Minute,
		MaxQueueAge: 15 * time.Minute,
	}
}

// VehicleSnapshot is the state an interlock decision is made against.
type VehicleSnapshot struct {
	SpeedKmh *float64
	FixAge   time.Duration
	HasFix   bool
}

// InterlockVerdict is the outcome, with a reason suitable for both the audit
// trail and the operator's screen.
type InterlockVerdict struct {
	Allowed bool
	Reason  string
}

// EvaluateInterlock decides whether a command may be delivered right now.
//
// Pure, so every refusal path is testable. Mobilise (releasing the engine) is
// always permitted — the dangerous direction is cutting power, and refusing to
// UNDO an immobilisation would strand a vehicle.
func EvaluateInterlock(kind string, queuedFor time.Duration, v VehicleSnapshot, cfg InterlockConfig) InterlockVerdict {
	if queuedFor > cfg.MaxQueueAge {
		return InterlockVerdict{false, fmt.Sprintf(
			"command expired after %s queued (limit %s)", queuedFor.Round(time.Second), cfg.MaxQueueAge)}
	}
	if kind == CommandMobilize {
		// Restoring the engine is always safe to deliver.
		return InterlockVerdict{true, "mobilize is always permitted"}
	}
	if kind != CommandImmobilize {
		return InterlockVerdict{false, "unsupported command kind: " + kind}
	}

	if !v.HasFix {
		return InterlockVerdict{false, "no position fix for this vehicle; refusing to immobilise blind"}
	}
	if v.FixAge > cfg.MaxFixAge {
		return InterlockVerdict{false, fmt.Sprintf(
			"last fix is %s old (limit %s); vehicle position is unknown",
			v.FixAge.Round(time.Second), cfg.MaxFixAge)}
	}
	if v.SpeedKmh == nil {
		return InterlockVerdict{false, "no speed reading; refusing to immobilise without knowing if the vehicle is moving"}
	}
	if *v.SpeedKmh > cfg.MaxSpeedKmh {
		return InterlockVerdict{false, fmt.Sprintf(
			"vehicle is moving at %.0f km/h (limit %.0f); immobilising at speed is unsafe",
			*v.SpeedKmh, cfg.MaxSpeedKmh)}
	}
	return InterlockVerdict{true, fmt.Sprintf("stationary at %.0f km/h with a %s-old fix",
		*v.SpeedKmh, v.FixAge.Round(time.Second))}
}
