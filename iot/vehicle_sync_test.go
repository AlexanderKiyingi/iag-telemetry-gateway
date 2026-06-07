package iot

import (
	"context"
	"testing"
)

func TestStatusSyncResult_unchangedMaintenance(t *testing.T) {
	speed := 30.0
	got := DeriveStatusFromPing("maintenance", &speed)
	if got != "maintenance" {
		t.Fatalf("got %q", got)
	}
}

func TestPublishStatusChange_skipsWhenUnchanged(t *testing.T) {
	t.Setenv("EVENT_BUS_ENABLED", "false")
	s := &Store{}
	if err := s.PublishStatusChange(context.Background(), StatusSyncResult{Changed: false}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}
