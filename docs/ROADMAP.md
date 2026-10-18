# Roadmap

hearth is a harness on top of coding agents, not a control panel over their terminals. It drives
each agent through a structured protocol and owns sessions, worktrees, permissions, tools and
orchestration itself; a backend is little more than a model loop.

Everything new lives in `core.App` as plain data, never in `tui`, so it reaches every client through
the daemon unchanged.

## Done

- **Daemon.** One background process owns the app; every `hearth` is a client of it over a Unix
  socket. Spec: `docs/specs/daemon.md`.
- **`start_agent`.** Subagents as hearth sessions on any backend, shown as tabs of the session that
  started them. Spec: `docs/specs/start-agent.md`.

## Next

### 1. Daemon follow-ups

- Reconnect a client without a full snapshot.
- Per-session subscriptions, so a client loads only the transcripts it shows.
- Notifications while nobody is attached. Reverses the daemon's "permissions wait, no
  notifications" decision.
- SSH over a forwarded socket.

### 2. Nested sessions

A session starts other sessions, they report back, and the parent wakes when they finish:
`start_session`, `send_message`, `list_sessions`, `get_session`, and children nested under their
parent in the sidebar. Needs research first; draft notes in `docs/specs/nested-sessions.md`.

### 3. Web client

A responsive PWA, served by a bridge in the hearth binary that relays the daemon's protocol over
WebSocket, reached over Tailscale.

- Bridge as a tailnet node (`tsnet`): TLS from MagicDNS, each connection checked against your
  Tailscale login with `WhoIs`.
- Web assets embedded in the binary, so client and daemon always speak the same protocol version.
- Web Push for finished turns and permission requests; payloads carry a title and state, never
  transcript text.
- Needs reconnect and per-session subscriptions from §1: a phone reconnects every time it's opened.

### 4. Backends

Goal: built-in tools off, hearth's tools in their place; a backend is a model loop plus our tools
behind our gate.

Every backend must start, resume, interrupt and queue mid-turn messages; emit structured events;
serve `core.Tools` and route every other tool through hearth's permissions; and accept appended
system-prompt text. Fork, rewind, unqueue, permission modes and model listing are optional
capabilities, kept as bools until a third backend shows the right shape.

## Deferred until it hurts

- Transcript grep loads every transcript on the UI goroutine; prefilter with ripgrep and parse only
  the hits, off the UI goroutine.
- Fuzzy matching is greedy; the markdown cache clears wholesale at 500 entries.
