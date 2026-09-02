# mc-console-bridge

A sidecar for the FWB Bedrock server pod that exposes the server console over
HTTP, behind a fixed command allowlist. It is the only component with write
access to the console — nothing upstream of it (including any LLM) can run
an arbitrary command.

## Why this exists

Mojang's Bedrock dedicated server has no plugin API, no RCON, and no admin
API. The only way to write to the running server is its console stdin. This
bridge speaks the console's real transport (`mc-server-runner`'s
`WEBSOCKET_CONSOLE`) and turns it into a small, auditable HTTP surface for
`minecraft-server-agent`.

## Console protocol (as verified against a live server)

`mc-server-runner`'s websocket console is **not** a raw command socket:

- Endpoint is `/console`, not the bind address's root path.
- Auth is via the `Sec-WebSocket-Protocol` header:
  `mc-server-runner-ws-v1, <password>` — not a query param, not
  `Authorization`.
- Messages are JSON: send `{"type":"stdin","data":"<command>\n"}`; the server
  broadcasts `{"type":"stdout"|"stderr","data":"..."}` for console output and
  `{"type":"logHistory","lines":[...]}"` once on connect.
- **Origin checking is on by default and rejects everything** unless the
  server sets `WEBSOCKET_DISABLE_ORIGIN_CHECK=true` (or configures
  `WEBSOCKET_ALLOWED_ORIGINS` to include whatever Origin this bridge sends,
  which by default is none). Since the console is bound to loopback
  (`127.0.0.1`) and never reachable from outside the pod, disabling the
  origin check is safe here and is a **required** part of the server's
  `extraEnv` wiring — see `jdw-deployments/charts/minecraft-fwb`.
- The protocol carries no request/response correlation id. `SendCommand`
  collects broadcast output for a short fixed window after writing a
  command; concurrent unrelated console activity can appear in that output.
  This is a protocol limitation, not fixable client-side.
- Observed: `say` produced no console output at all with zero players
  online (reproduced twice); `list` round-tripped correctly. Flagging as a
  known quirk — announcements to an empty server may be silent.

## Allowlist

`POST /command` accepts only commands matching one of these templates
(`allowlist.go`); anything else is refused with `403` and never reaches the
console:

| Template | Example |
|---|---|
| `list` | `list` |
| `say <text>` | `say hello` |
| `title <target> <title\|subtitle\|actionbar\|clear\|reset> [...]` | `title @a actionbar Hi` |
| `tellraw <target> <json with "rawtext">` | `tellraw @a {"rawtext":[{"text":"hi"}]}` |
| `time query <day\|daytime\|gametime>` | `time query day` |
| `gamerule <name>` (no value — read-only query) | `gamerule doDaylightCycle` |

Commands containing a newline or carriage return are refused outright, so
one HTTP request cannot smuggle a second console line.

## Permissions

`GET /permissions` parses `<DATA_DIR>/permissions.json` into
`{xuid: "operator"|"member"|"visitor"}`. An XUID **absent** from that file is
not "visitor" — it falls back to the server's
`default-player-permission-level` (in `server.properties`, not read by this
service). Callers must treat a missing XUID as unknown, not as a specific
level.

`GET /allowlist` parses `<DATA_DIR>/allowlist.json`.

## Environment variables

| Var | Required | Default | Meaning |
|---|---|---|---|
| `BRIDGE_TOKEN` | yes | — | Bearer token required on every `/command`, `/permissions`, `/allowlist`, `/events` call |
| `CONSOLE_PASSWORD` | yes | — | Must match the server's `WEBSOCKET_PASSWORD` |
| `HTTP_ADDR` | no | `:8080` | Bridge's own HTTP bind address |
| `CONSOLE_ADDR` | no | `127.0.0.1:8765` | Server's websocket console address |
| `DATA_DIR` | no | `/data` | Mounted server data volume (read-only) |

## Endpoints

| Route | Auth | Purpose |
|---|---|---|
| `POST /command` | bearer | `{"command": "..."}` → allowlist check → console → `{"rule": "...", "output": "..."}` |
| `GET /permissions` | bearer | Parsed `permissions.json` |
| `GET /allowlist` | bearer | Parsed `allowlist.json` |
| `GET /events` | bearer | Typed server log events (stdout source wiring is Stage 2 scope; returns `[]` for now) |
| `GET /healthz` | none | Liveness |
| `GET /metrics` | none | Prometheus |

## Status

Stage 0/1 scaffold: allowlist, permissions/allowlist parsing, console
websocket client, and HTTP surface are implemented and tested. `/events`'
real stdout source, and the event-pattern regexes for crash/content-log
detection, need validation against real server logs before they're trusted
in production — see comments in `events.go`.
