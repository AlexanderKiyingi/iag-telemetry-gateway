// Command gateway listens for Teltonika Codec 8/8E TCP connections (default :5027).
//
//	DATABASE_URL=postgres://... IOT_ADDR=:5027 go run ./cmd/gateway
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/iag/fleet-iot/iot"
	"github.com/iag/fleet-iot/pg"
)

func main() {
	configureLogger()
	addr := os.Getenv("IOT_ADDR")
	if addr == "" {
		addr = ":5027"
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	registryPool, telemetryPool, err := pg.ConnectSplit(connectCtx)
	cancel()
	if err != nil {
		slog.Error("connect Postgres", "err", err)
		os.Exit(1)
	}
	defer registryPool.Close()
	if telemetryPool != registryPool {
		defer telemetryPool.Close()
	}

	store := iot.NewSplitStore(registryPool, telemetryPool)
	if os.Getenv("REGISTRY_DATABASE_URL") != "" {
		slog.Info("telemetry TCP gateway: split DB (registry + telemetry)")
	}
	hub := iot.NewHubFromEnv()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}
	slog.Info("telemetry TCP gateway listening", "addr", addr)

	srv := &tcpGateway{store: store, hub: hub, sem: make(chan struct{}, maxTCPConns)}
	go srv.serve(listener)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	_ = listener.Close()
	done := make(chan struct{})
	go func() {
		srv.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

func configureLogger() {
	var h slog.Handler
	if os.Getenv("LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(h))
}

// maxTCPConns caps concurrently-handled device connections so an attacker
// opening many sockets cannot exhaust goroutines/file descriptors. Connections
// beyond the cap are rejected immediately rather than queued.
const maxTCPConns = 2048

type tcpGateway struct {
	store *iot.Store
	hub   *iot.Hub
	wg    sync.WaitGroup
	sem   chan struct{}
}

func (g *tcpGateway) serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("accept failed", "err", err)
			continue
		}
		select {
		case g.sem <- struct{}{}:
			g.wg.Add(1)
			go func() {
				defer g.wg.Done()
				defer func() { <-g.sem }()
				g.handle(conn)
			}()
		default:
			slog.Warn("tcp gateway at capacity; rejecting connection",
				"remote", conn.RemoteAddr().String(), "max", maxTCPConns)
			_ = conn.Close()
		}
	}
}

func (g *tcpGateway) handle(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	defer conn.Close()
	// A malformed Teltonika frame that slips past the codec's bounds checks must
	// not panic the whole process (an unrecovered panic in any goroutine is
	// fatal in Go). Contain it to this one connection.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("tcp gateway connection panic recovered", "remote", remote, "panic", rec)
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(conn)
	imei, err := iot.ReadHandshake(r)
	if err != nil {
		return
	}
	logger := slog.With("remote", remote, "imei", imei)

	hsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	device, err := g.store.FindBySerial(hsCtx, imei)
	if err != nil {
		cancel()
		_ = iot.WriteHandshakeResponse(conn, false)
		return
	}
	if err := iot.WriteHandshakeResponse(conn, true); err != nil {
		cancel()
		return
	}
	if err := g.store.MarkSeen(hsCtx, device.ID, ipOnly(remote)); err != nil {
		logger.Warn("mark device seen failed", "err", err)
	}
	cancel()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		codec, records, err := iot.ReadAVLPacket(r)
		if err != nil {
			return
		}
		pings := make([]iot.Ping, 0, len(records))
		for _, rec := range records {
			pings = append(pings, recordToPing(rec, device))
		}

		// Bound each packet's database work so a slow/hung DB cannot pin the
		// connection (and its goroutine slot) indefinitely.
		opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := g.store.InsertPings(opCtx, pings); err != nil {
			opCancel()
			logger.Error("insert pings failed", "err", err)
			return
		}
		if device.VehicleID != "" && len(pings) > 0 {
			newest := pings[0]
			for _, p := range pings[1:] {
				if p.TS.After(newest.TS) {
					newest = p
				}
			}
			if _, err := g.store.ApplyVehicleHotState(opCtx, newest); err != nil {
				logger.Warn("registry sync failed after TCP ingest",
					"vehicleId", device.VehicleID, "err", err)
			}
			if err := g.store.ApplyGeofenceTransitions(opCtx, iot.ProcessGeofences(newest)); err != nil {
				logger.Warn("geofence transitions failed after TCP ingest", "err", err)
			}
		}
		if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
			logger.Warn("mark device seen failed", "err", err)
		}
		opCancel()

		_ = iot.WriteACK(conn, len(records))
		if g.hub != nil {
			for _, p := range pings {
				g.hub.Publish(p)
			}
		}
		logger.Info("pings persisted", "count", len(records), "codec", codec)
	}
}

const (
	ioIDIgnition   uint16 = 239
	ioIDOdoMeters  uint16 = 199
	ioIDFuelPct    uint16 = 89
)

func recordToPing(rec iot.AVLRecord, device *iot.Device) iot.Ping {
	devID := device.ID
	alt := float64(rec.Altitude)
	heading := float64(rec.Angle)
	sats := int(rec.Satellites)
	p := iot.Ping{
		VehicleID: device.VehicleID, DeviceID: &devID, TS: rec.Timestamp,
		Lat: rec.Latitude, Lng: rec.Longitude, Altitude: &alt, Heading: &heading, Satellites: &sats,
	}
	if rec.Speed != 0xFFFF {
		sp := float64(rec.Speed)
		p.SpeedKmh = &sp
	}
	if v, ok := rec.IOs[ioIDOdoMeters]; ok {
		odoKm := float64(v) / 1000.0
		p.Odo = &odoKm
	}
	if v, ok := rec.IOs[ioIDFuelPct]; ok {
		pct := float64(v) / 10.0
		p.FuelLevel = &pct
	}
	if v, ok := rec.IOs[ioIDIgnition]; ok {
		on := v != 0
		p.Ignition = &on
	}
	if rec.EventIOID != 0 {
		ev := int(rec.EventIOID)
		p.EventID = &ev
	}
	p.Raw = encodeIOMap(rec.IOs)
	return p
}

func encodeIOMap(m map[uint16]int64) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[strconv.Itoa(int(k))] = v
	}
	b, _ := json.Marshal(out)
	return b
}

func ipOnly(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
