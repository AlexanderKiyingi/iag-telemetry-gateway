package iot

// The shared post-ingest pipeline.
//
// Every protocol gateway ends up doing the same seven things with a batch of
// pings: persist them, sync vehicle hot-state, evaluate geofences, evaluate
// speed, refresh last-seen, fan out to the live hub, and deliver any queued
// device command. Until now each gateway spelled that out in its own main.go.
//
// That cost real bugs, twice. Server-side overspeed shipped in the SinoTrack
// gateway and Teltonika vehicles silently had no speed monitoring. Then
// database-backed geofences shipped the same way, and whether a depot existed
// depended on which protocol a tracker happened to speak — a difference nobody
// would think to look for.
//
// Both were the same mistake: per-binary wiring that a new feature has to be
// remembered in N times. A gateway now decodes its wire format, builds Pings,
// and hands them here. Adding a third protocol gets the whole feature set by
// construction rather than by diligence.

import (
	"context"
	"log/slog"
	"time"
)

// PipelineStore is everything the pipeline touches. An interface rather than
// *Store so gateway tests keep using their fakes.
type PipelineStore interface {
	InsertPings(ctx context.Context, pings []Ping) (int, error)
	ApplyVehicleHotState(ctx context.Context, p Ping) (StatusSyncResult, error)
	ApplyGeofenceTransitions(ctx context.Context, transitions []GeofenceTransition) error
	ApplyOverspeed(ctx context.Context, p Ping, cfg OverspeedConfig) error
	MarkSeen(ctx context.Context, deviceID int64, ip string) error
}

// CommandSink is the device's own connection, written to when a queued command
// clears its interlocks. net.Conn satisfies it.
//
// Delivery is attempted on the live socket because these devices dial out —
// there is no way to call them. The moment a device is talking is the only
// moment it can be reached.
type CommandSink interface {
	Write(p []byte) (int, error)
	SetWriteDeadline(t time.Time) error
}

// CommandStore is the queue half, kept separate so a gateway that does not
// support downlink simply passes nil.
type CommandStore interface {
	NextPendingCommand(ctx context.Context, deviceID int64) (*DeviceCommand, error)
	VehicleSnapshotFor(ctx context.Context, vehicleID string) (VehicleSnapshot, error)
	MarkCommandSent(ctx context.Context, id int64, payload string, snap VehicleSnapshot) error
	MarkCommandRefused(ctx context.Context, id int64, reason string, snap VehicleSnapshot) error
}

// Pipeline holds the per-gateway configuration.
type Pipeline struct {
	Store     PipelineStore
	Hub       *Hub
	Overspeed OverspeedConfig

	// Commands is nil on gateways without a verified downlink path.
	Commands  CommandStore
	Interlock InterlockConfig
	// CommandsEnabled gates delivery entirely. Even when true, nothing is sent
	// without a registered per-model encoder.
	CommandsEnabled bool
}

// IngestResult reports what happened, for the gateway's logging.
type IngestResult struct {
	Inserted      int
	StatusChanged bool
	NewStatus     string
}

// Ingest runs the full post-decode pipeline for one batch of pings.
//
// Error semantics match what the gateways did individually: a failed insert is
// returned (the caller drops the connection so the device retries), while the
// downstream steps warn and continue. Losing a geofence transition is not a
// reason to discard telemetry that is already persisted.
//
// pings must be non-empty and already filtered for usable positions; the
// gateway owns that because "usable" is protocol-specific.
func (p *Pipeline) Ingest(ctx context.Context, device *Device, pings []Ping, clientIP string, logger *slog.Logger) (IngestResult, error) {
	var res IngestResult
	if len(pings) == 0 || device == nil {
		return res, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	n, err := p.Store.InsertPings(ctx, pings)
	if err != nil {
		return res, err
	}
	res.Inserted = n

	newest := newestPing(pings)
	if newest == nil {
		return res, nil
	}

	sync, err := p.Store.ApplyVehicleHotState(ctx, *newest)
	if err != nil {
		logger.Warn("registry sync failed after ingest", "vehicleId", newest.VehicleID, "err", err)
	} else {
		res.StatusChanged, res.NewStatus = sync.Changed, sync.NewStatus
	}

	if err := p.Store.ApplyGeofenceTransitions(ctx, ProcessGeofences(*newest)); err != nil {
		logger.Warn("geofence transitions failed after ingest", "vehicleId", newest.VehicleID, "err", err)
	}
	if err := p.Store.ApplyOverspeed(ctx, *newest, p.Overspeed); err != nil {
		logger.Warn("overspeed evaluation failed after ingest", "vehicleId", newest.VehicleID, "err", err)
	}
	if err := p.Store.MarkSeen(ctx, device.ID, clientIP); err != nil {
		logger.Warn("mark device seen failed", "err", err)
	}

	if p.Hub != nil {
		for _, ping := range pings {
			p.Hub.Publish(ping)
		}
	}
	return res, nil
}

// DeliverPendingCommand checks for a queued command and, if the interlocks
// allow and a verified encoder exists, writes it to the device's socket.
//
// The interlock runs HERE, against the vehicle's state at this instant, rather
// than when the operator pressed the button: a command can sit queued while the
// truck pulls onto a highway, so validating at request time proves nothing.
//
// Every outcome is recorded, refusals included — "why did nothing happen" and
// "who tried to stop this truck" both need answering after the fact.
func (p *Pipeline) DeliverPendingCommand(ctx context.Context, sink CommandSink, device *Device, logger *slog.Logger) {
	if !p.CommandsEnabled || p.Commands == nil || sink == nil || device == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}

	cmd, err := p.Commands.NextPendingCommand(ctx, device.ID)
	if err != nil {
		logger.Warn("check pending command failed", "err", err)
		return
	}
	if cmd == nil {
		return
	}

	snap, err := p.Commands.VehicleSnapshotFor(ctx, cmd.VehicleID)
	if err != nil {
		// Leave it pending: refusing on a database blip would be wrong, and
		// delivering without a snapshot would be far worse.
		logger.Warn("vehicle snapshot for interlock failed", "err", err)
		return
	}

	if verdict := EvaluateInterlock(cmd.Kind, time.Since(cmd.RequestedAt), snap, p.Interlock); !verdict.Allowed {
		logger.Warn("command refused by interlock",
			"commandId", cmd.ID, "kind", cmd.Kind, "reason", verdict.Reason)
		if err := p.Commands.MarkCommandRefused(ctx, cmd.ID, verdict.Reason, snap); err != nil {
			logger.Warn("record command refusal failed", "err", err)
		}
		return
	}

	// No default encoder exists. An unrecognised model is refused rather than
	// sent a guessed byte sequence.
	payload, err := EncodeCommand(device.Model, cmd.Kind)
	if err != nil {
		logger.Warn("command refused: no verified encoder",
			"commandId", cmd.ID, "model", device.Model, "err", err)
		if err := p.Commands.MarkCommandRefused(ctx, cmd.ID, err.Error(), snap); err != nil {
			logger.Warn("record command refusal failed", "err", err)
		}
		return
	}

	_ = sink.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := sink.Write(payload); err != nil {
		// Stays pending; the device will reconnect and we try again.
		logger.Error("command write failed", "commandId", cmd.ID, "err", err)
		return
	}
	_ = sink.SetWriteDeadline(time.Time{})

	logger.Info("command delivered", "commandId", cmd.ID, "kind", cmd.Kind)
	if err := p.Commands.MarkCommandSent(ctx, cmd.ID, string(payload), snap); err != nil {
		logger.Warn("record command sent failed", "err", err)
	}
}
