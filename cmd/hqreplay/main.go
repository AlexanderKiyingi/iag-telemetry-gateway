// Command hqreplay is a development tool that feeds SinoTrack / HQ-protocol
// frames to a running cmd/sinotrack gateway over TCP — so the ingest path can
// be smoke-tested without physical tracker hardware.
//
// It either replays frames verbatim from a file (one frame per line) or, by
// default, generates a stream of synthetic V1 position reports that drift
// slightly around a start coordinate with current UTC timestamps.
//
// Provision first: the gateway authenticates on the device id, so create an
// iot_devices row whose serial equals -id and bind it to a vehicle, e.g.
//
//	INSERT INTO iot_devices (serial, label, vehicle_id, is_active)
//	VALUES ('9170503816', 'sinotrack smoke test', '<vehicle-id>', true);
//
// Then, with the gateway listening on :5013:
//
//	go run ./cmd/hqreplay -addr localhost:5013 -id 9170503816 -count 10
//	go run ./cmd/hqreplay -addr localhost:5013 -file captured-frames.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", "localhost:5013", "gateway address host:port")
		id       = flag.String("id", "9170503816", "device id sent in each frame (must match iot_devices.serial)")
		lat      = flag.Float64("lat", 0.347596, "start latitude, decimal degrees (default: Kampala)")
		lng      = flag.Float64("lng", 32.582520, "start longitude, decimal degrees")
		speed    = flag.Float64("speed", 24.0, "speed over ground, knots")
		count    = flag.Int("count", 5, "number of synthetic frames to send (ignored with -file)")
		interval = flag.Duration("interval", 2*time.Second, "delay between frames")
		file     = flag.String("file", "", "replay frames verbatim from this file (one per line) instead of generating")
	)
	flag.Parse()

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		log.Fatalf("dial %s: %v", *addr, err)
	}
	defer conn.Close()
	log.Printf("connected to %s", *addr)

	if *file != "" {
		if err := replayFile(conn, *file, *interval); err != nil {
			log.Fatalf("replay %s: %v", *file, err)
		}
		return
	}

	curLat, curLng := *lat, *lng
	for i := 0; i < *count; i++ {
		frame := buildV1Frame(*id, curLat, curLng, *speed, time.Now().UTC())
		if _, err := conn.Write([]byte(frame)); err != nil {
			log.Fatalf("write frame %d: %v", i, err)
		}
		log.Printf("sent %d/%d: %s", i+1, *count, frame)
		// Drift ~11 m north-east per frame so the track and geofence logic move.
		curLat += 0.0001
		curLng += 0.0001
		if i < *count-1 {
			time.Sleep(*interval)
		}
	}
	log.Printf("done")
}

func replayFile(conn net.Conn, path string, interval time.Duration) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue // blank line or comment
		}
		if _, err := conn.Write([]byte(line)); err != nil {
			return fmt.Errorf("write line %d: %w", n+1, err)
		}
		n++
		log.Printf("sent %d: %s", n, line)
		time.Sleep(interval)
	}
	log.Printf("replayed %d frames", n)
	return sc.Err()
}

// buildV1Frame renders a valid HQ V1 position report for the given decimal
// coordinates — the inverse of iot.ParseHQFrame's decoding. Latitude is
// DDMM.MMMM, longitude DDDMM.MMMM; date is DDMMYY, time HHMMSS (UTC).
func buildV1Frame(id string, lat, lng, speedKnots float64, t time.Time) string {
	latStr, ns := encodeCoord(lat, 2, "N", "S")
	lngStr, ew := encodeCoord(lng, 3, "E", "W")
	return fmt.Sprintf("*HQ,%s,V1,%s,A,%s,%s,%s,%s,%05.2f,%03d,%s,FFFFFBFF#",
		id,
		t.Format("150405"), // HHMMSS
		latStr, ns,
		lngStr, ew,
		speedKnots,
		0,                  // heading, degrees
		t.Format("020106"), // DDMMYY
	)
}

// encodeCoord turns decimal degrees into the HQ DDMM.MMMM / DDDMM.MMMM string
// plus hemisphere. degDigits is 2 for latitude, 3 for longitude.
func encodeCoord(dec float64, degDigits int, pos, neg string) (string, string) {
	hemi := pos
	if dec < 0 {
		hemi = neg
		dec = -dec
	}
	deg := math.Trunc(dec)
	minutes := (dec - deg) * 60
	// value = DDD*100 + MM.MMMM; total width = degDigits + 2 (mins) + 1 (dot) + 4 (frac).
	value := deg*100 + minutes
	width := degDigits + 2 + 1 + 4
	return fmt.Sprintf("%0*.4f", width, value), hemi
}
