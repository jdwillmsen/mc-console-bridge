package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConsole() *Console {
	return NewConsole("127.0.0.1:0", "pw", 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestConsoleIngestEvents_LineSplitAcrossFrames(t *testing.T) {
	c := testConsole()

	c.ingestEvents(wsMessage{Type: "stdout", Data: "[INFO] Player conn"})
	if got := c.Events.Since(0); len(got) != 0 {
		t.Fatalf("partial line produced %d events, want 0: %+v", len(got), got)
	}

	c.ingestEvents(wsMessage{Type: "stdout", Data: "ected: Steve, xuid: 111\n"})

	got := c.Events.Since(0)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(got), got)
	}
	if got[0].Type != EventConnect || got[0].Player != "Steve" {
		t.Errorf("event = %+v, want connect for Steve", got[0])
	}
	if got[0].Raw != "[INFO] Player connected: Steve, xuid: 111" {
		t.Errorf("raw = %q, want the rejoined line", got[0].Raw)
	}
}

func TestConsoleIngestEvents_StdoutAndStderrResidualsAreIndependent(t *testing.T) {
	c := testConsole()

	c.ingestEvents(wsMessage{Type: "stdout", Data: "Player connected: Steve"})
	c.ingestEvents(wsMessage{Type: "stderr", Data: "Player disconnected: Alex"})
	c.ingestEvents(wsMessage{Type: "stdout", Data: ", xuid: 111\n"})
	c.ingestEvents(wsMessage{Type: "stderr", Data: ", xuid: 222\n"})

	got := c.Events.Since(0)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Type != EventConnect || got[0].Player != "Steve" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].Type != EventDisconnect || got[1].Player != "Alex" {
		t.Errorf("event 1 = %+v", got[1])
	}
}

func TestConsoleIngestEvents_DropsOversizedPartialLine(t *testing.T) {
	c := testConsole()

	c.ingestEvents(wsMessage{Type: "stdout", Data: strings.Repeat("x", maxResidualLine+1)})
	if c.residual["stdout"] != "" {
		t.Fatalf("residual retained %d bytes, want it dropped", len(c.residual["stdout"]))
	}

	c.ingestEvents(wsMessage{Type: "stdout", Data: "Player connected: Steve, xuid: 111\n"})
	if got := c.Events.Since(0); len(got) != 1 {
		t.Fatalf("got %d events after drop, want 1: %+v", len(got), got)
	}
}

func TestConsoleSendCommand_NotConnected(t *testing.T) {
	c := testConsole()
	if _, err := c.SendCommand(t.Context(), "list"); err != ErrNotConnected {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

func testServer(t *testing.T, c *Console) http.Handler {
	t.Helper()
	return newMux(&server{
		cfg:     Config{BridgeToken: "tok"},
		console: c,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestReadyzReflectsConsoleConnection(t *testing.T) {
	c := testConsole()
	mux := testServer(t, c)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disconnected /readyz = %d, want 503", rec.Code)
	}

	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("connected /readyz = %d, want 200", rec.Code)
	}
}

func TestHealthzStaysGreenWhileConsoleIsDown(t *testing.T) {
	mux := testServer(t, testConsole())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rec.Code)
	}
}

func TestCommandRejectsOversizedBody(t *testing.T) {
	mux := testServer(t, testConsole())

	body := `{"command":"say ` + strings.Repeat("a", maxCommandBodyBytes) + `"}`
	req := httptest.NewRequest("POST", "/command", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body = %d, want 400", rec.Code)
	}
}
