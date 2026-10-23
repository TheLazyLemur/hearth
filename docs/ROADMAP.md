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
- **Web client.** `hearth web [addr]` serves a phone-first page, embedded in the binary, on
  `127.0.0.1:7420` by default. Each tab gets a daemon connection of its own, relayed over a
  WebSocket one message per frame. hearth assumes nothing about what sits in front of it: TLS,
  a proxy or a tailnet are the user's to add, so a browser gets in only by pairing with the token
  in `HEARTH_HOME/web.token`, and the socket refuses other origins. Sessions, transcripts, agent
  tabs, sending, stopping, permission and question cards, new sessions in known folders, and a
  sheet for each session's backend, model, effort and permission mode.
- **Shell panel.** One shell per session, in a side panel toggled with ctrl+t. The daemon owns
  the pty and its screen, so a shell outlives `:q` and every instance shows it, as in a
  multiplexer. The instance that last focused it sets its size.

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

### 3. Web client follow-ups

- Web Push for finished turns and permission requests; payloads carry a title and state, never
  transcript text. Needs a manifest and service worker, so HTTPS from whatever fronts it.
- Reconnect and per-session subscriptions from §1: a phone reconnects every time it's opened,
  and each time takes a full snapshot.
- Rewind, unqueue and chips; the shell panel.
- New sessions in a typed folder, once the daemon checks the folder exists.

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
