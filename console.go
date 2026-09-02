package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// consoleEndpoint and the auth subprotocol name are dictated by
// itzg/mc-server-runner's websocket_shell_service.go and are not
// configurable server-side beyond the password.
const (
	consoleEndpoint = "/console"
	authSubproto    = "mc-server-runner-ws-v1"

	// commandCollectWindow is how long SendCommand waits for stdout/stderr
	// broadcasts after writing a command, before returning whatever arrived.
	// The protocol carries no request/response correlation id, so output
	// collected in this window may include unrelated concurrent console
	// activity. That is an inherent limit of the console protocol, not a
	// bridge bug; callers should treat the result as best-effort.
	commandCollectWindow = 800 * time.Millisecond

	minReconnectDelay = 1 * time.Second
	maxReconnectDelay = 30 * time.Second
)

// wsMessage mirrors the JSON envelope mc-server-runner uses on the wire.
type wsMessage struct {
	Type   string   `json:"type"`
	Data   string   `json:"data,omitempty"`
	Lines  []string `json:"lines,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Console maintains a reconnecting websocket connection to the bedrock
// server's console and serializes commands through it. It also feeds
// Events: mc-server-runner pushes every console stdout/stderr line and a
// logHistory backfill over this same connection, so no separate log-tailing
// mechanism is needed for GET /events.
type Console struct {
	addr     string
	password string
	logger   *slog.Logger

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool

	subMu       sync.Mutex
	subscribers map[chan wsMessage]struct{}

	Events *EventLog
}

func NewConsole(addr, password string, logger *slog.Logger) *Console {
	return &Console{
		addr:        addr,
		password:    password,
		logger:      logger,
		subscribers: make(map[chan wsMessage]struct{}),
		Events:      NewEventLog(),
	}
}

// Run connects and reconnects with backoff until ctx is cancelled.
func (c *Console) Run(ctx context.Context) {
	delay := minReconnectDelay
	for {
		if ctx.Err() != nil {
			return
		}

		err := c.connectAndRead(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logger.Warn("console connection lost", "error", err)
		}

		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()

		jitter := time.Duration(rand.Int63n(int64(delay) / 2))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay + jitter):
		}
		if delay < maxReconnectDelay {
			delay *= 2
			if delay > maxReconnectDelay {
				delay = maxReconnectDelay
			}
		}
	}
}

func (c *Console) connectAndRead(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("ws://%s%s", c.addr, consoleEndpoint)
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Sec-WebSocket-Protocol": {authSubproto + ", " + c.password},
		},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("console auth rejected: %w", err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()

	c.logger.Info("console connected", "addr", c.addr)

	for {
		var msg wsMessage
		if err := readJSON(ctx, conn, &msg); err != nil {
			return err
		}
		if msg.Type == "authFailure" {
			return fmt.Errorf("console auth failure: %s", msg.Reason)
		}
		c.ingestEvents(msg)
		c.broadcast(msg)
	}
}

// ingestEvents feeds Events from a console broadcast: the one-shot
// logHistory backfill on connect, and every stdout/stderr line as it
// arrives. Received-at time is used for all events, including backfilled
// history, since the raw line's own timestamp format isn't parsed here.
func (c *Console) ingestEvents(msg wsMessage) {
	now := time.Now()
	switch msg.Type {
	case "logHistory":
		for _, line := range msg.Lines {
			c.Events.Ingest(line, now)
		}
	case "stdout", "stderr":
		for _, line := range strings.Split(msg.Data, "\n") {
			if line == "" {
				continue
			}
			c.Events.Ingest(line, now)
		}
	}
}

func (c *Console) broadcast(msg wsMessage) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch := range c.subscribers {
		select {
		case ch <- msg:
		default:
			// Slow subscriber: drop rather than block the read loop.
		}
	}
}

func (c *Console) subscribe() (chan wsMessage, func()) {
	ch := make(chan wsMessage, 32)
	c.subMu.Lock()
	c.subscribers[ch] = struct{}{}
	c.subMu.Unlock()
	return ch, func() {
		c.subMu.Lock()
		delete(c.subscribers, ch)
		c.subMu.Unlock()
		close(ch)
	}
}

// Connected reports whether the console websocket is currently established.
func (c *Console) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// ErrNotConnected is returned by SendCommand when no console connection is
// currently established.
var ErrNotConnected = fmt.Errorf("console not connected")

// SendCommand writes a command to the console and collects any stdout/stderr
// broadcasts that follow within commandCollectWindow. The output is
// best-effort: the protocol has no correlation id, so concurrent console
// activity from other sources can appear in the result.
func (c *Console) SendCommand(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	conn := c.conn
	connected := c.connected
	c.mu.Unlock()
	if !connected || conn == nil {
		return "", ErrNotConnected
	}

	sub, unsub := c.subscribe()
	defer unsub()

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeJSON(writeCtx, conn, wsMessage{Type: "stdin", Data: cmd + "\n"}); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}

	var out []byte
	deadline := time.After(commandCollectWindow)
	for {
		select {
		case msg := <-sub:
			if msg.Type == "stdout" || msg.Type == "stderr" {
				out = append(out, msg.Data...)
			}
		case <-deadline:
			return string(out), nil
		case <-ctx.Done():
			return string(out), ctx.Err()
		}
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v *wsMessage) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v wsMessage) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}
