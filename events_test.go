package main

import (
	"strings"
	"testing"
)

func TestParseEvents(t *testing.T) {
	log := strings.Join([]string{
		`[2026-09-02 04:35:59:427 INFO] Player connected: Steve, xuid: 2535457893448396`,
		`[2026-09-02 04:36:10:001 INFO] Player disconnected: Steve, xuid: 2535457893448396`,
		`[2026-09-02 04:36:15:002 INFO] some unrelated line`,
		`terminate called after throwing an instance of 'std::length_error'`,
	}, "\n")

	events, err := ParseEvents(strings.NewReader(log))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Type != EventConnect || events[0].Player != "Steve" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Type != EventDisconnect || events[1].Player != "Steve" {
		t.Errorf("event 1 = %+v", events[1])
	}
	if events[2].Type != EventCrash {
		t.Errorf("event 2 = %+v, want crash", events[2])
	}
}

func TestParseEvents_Empty(t *testing.T) {
	events, err := ParseEvents(strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}
