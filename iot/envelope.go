package iot

import (
	"time"

	"github.com/google/uuid"
)

// PlatformEvent mirrors the fleet API outbox envelope (@iag/events shape).
type PlatformEvent struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Time          string         `json:"time"`
	Source        string         `json:"source"`
	SpecVersion   string         `json:"specversion"`
	CorrelationID string         `json:"correlationId,omitempty"`
	CausationID   string         `json:"causationId,omitempty"`
	Data          map[string]any `json:"data"`
}

func newPlatformEvent(eventType string, data map[string]any) PlatformEvent {
	return PlatformEvent{
		ID:          uuid.NewString(),
		Type:        eventType,
		Time:        time.Now().UTC().Format(time.RFC3339Nano),
		Source:      fleetEventSource,
		SpecVersion: fleetEventSpecVersion,
		Data:        data,
	}
}
