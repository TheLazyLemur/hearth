# hearth daemon

## Problem Statement

Running more than one hearth at a time is unsafe. Each instance has its own `core.App` and writes
session JSON to the same store, so the last writer wins, and two instances can resume the same CLI
session at once. Every instance shows a different picture of the same sessions. Quitting hearth
kills every agent it started, so a long turn can't outlive the terminal it was started in.

## Solution

A single background process, `hearth daemon`, owns `core.App`, the agent processes and the store.
Every `hearth` TUI is a client of it over a Unix socket: it receives a snapshot, then a stream of
changes, and sends commands as requests. All open instances show the same live state. `:q` only
detaches the client, so running turns carry on. The daemon starts itself when `hearth` is run and
exits once nobody is attached and nothing is running.

## User Stories

1. As a user, I want to open hearth in a second terminal and see the same sessions, so that I can work from wherever I am.
2. As a user, I want a reply streaming in one terminal to stream in every other open hearth, so that no instance shows stale state.
3. As a user, I want a message I send from one instance to appear in every other instance, so that the transcript is the same everywhere.
4. As a user, I want to `:q` while a turn is running and have it keep running, so that closing a terminal doesn't kill an agent's work.
5. As a user, I want to reopen hearth after `:q` and find the turn finished (or still streaming), so that I can pick up where it got to.
6. As a user, I want a permission request raised while nobody is attached to wait until I next open hearth, so that the agent doesn't proceed without me.
7. As a user, I want a pending permission to show in every attached instance, so that I can answer from whichever terminal I'm in.
8. As a user, I want the first answer to a permission to win and every other instance's dialog to close, so that a permission is never answered twice.
9. As a user, I want a second answer to an already-answered permission to say "already answered", so that I know my answer had no effect.
10. As a user, I want each instance to keep its own selection, focus, collapsed groups, scroll and visual mode, so that two terminals don't fight over what I'm looking at.
11. As a user, I want hearth launched in a folder to focus that folder's project, so that opening hearth in a repo still takes me to its sessions.
12. As a user, I want plain `hearth` to start the daemon when none is running, so that I never have to start it by hand.
13. As a user, I want the daemon to exit on its own after about 30 seconds with no clients and nothing running, so that it doesn't linger.
14. As a user, I want the daemon to stay alive while a turn runs, a permission waits or a message is queued, even with no client attached, so that work is never dropped by the idle exit.
15. As a user, I want a new hearth binary to replace an idle daemon of a different version automatically, so that upgrades just work.
16. As a user, I want a new hearth binary to refuse to attach to a different-version daemon that is running turns, with "daemon vX is running turns; finish or `hearth daemon stop`", so that an upgrade never kills work silently.
17. As a user, I want `hearth daemon stop` to stop the daemon and every agent, and print what was running, so that I know what I killed.
18. As a user, I want `hearth daemon status` to show PID, start time, version, client count and running sessions, so that I can see what the daemon is doing and which environment it was started in.
19. As a user, I want `hearth daemon` to run in the foreground, so that I can debug it and read its log live.
20. As a user, I want a client to exit with "daemon exited, see ~/.hearth/daemon.log" when the daemon dies, so that I know where to look.
21. As a user, I want a client that can't keep up to be disconnected with "disconnected: too slow", so that one stuck terminal can't stall the others.
22. As a user, I want errors raised by the daemon (save failures, agent errors, model listing errors) shown in every instance, so that I see them wherever I am.
23. As a user, I want errors from my own input (bad commands, copy confirmations) shown only in the instance I typed in, so that other terminals aren't cluttered.
24. As a user, I want opening a session's transcript in one instance to load it for all, so that switching terminals doesn't reload it.
25. As a user, I want transcript grep to keep working, so that searching across sessions doesn't regress.
26. As a user, I want rewind, unqueue and rewind preview to work as before, so that the daemon changes nothing about how a session is driven.
27. As a user, I want accepting a task chip in one instance to start the session in all, so that a chip can't be accepted twice.
28. As a user, I want model listing, model, effort, mode and backend changes to show in every instance, so that settings agree everywhere.
29. As a user, I want only processes running as me to be able to connect to the socket, so that another local user can't drive my agents.
30. As a user, I want the daemon's socket, lock and log to live under `HEARTH_HOME`, so that a separate `HEARTH_HOME` gets a separate daemon.
31. As a developer, I want the protocol not to assume the client can read the daemon's filesystem, so that SSH over a forwarded socket can come later without a protocol change.
32. As a developer, I want the change message to be generic per session, so that nested-session fields can be added without changing the protocol.

## Implementation Decisions

### Ownership

- The daemon is the only owner of `core.App`, the agent processes and the store. Clients never touch
  the store or start agents.
- One daemon goroutine owns `App`. It selects over agent events and client requests, the same loop
  the TUI runs today. `Handle` runs only there, side effects included (saves, title requests, agent
  calls).
- `Live` already splits plain data from live handles. The plain data goes to clients: `Entries`,
  `Running`, `Perms`, `Chips`, `Queued`, `Started`, `Verb`, `Version`, `Model`, and the loaded and
  errored flags that `Status` and `EnsureLoaded` rely on. The agent handle, the `pending` reply
  callbacks and the `TaskSuggested`/`TaskDismissed` `Reply` closures stay in the daemon; those
  closures are answered inside `Handle`, so they never cross the socket.

### Wire format and transport

- Newline-delimited JSON (one object per line) over a Unix socket, standard library only.
- The socket is `daemon.sock` under `HEARTH_HOME` (default `~/.hearth`), in a `0700` directory. Each
  connection's peer UID must match the daemon's (`getpeereid`); otherwise it is closed. No token.
- Handshake: the client sends its version first. On an exact match the daemon replies with the
  snapshot. On a mismatch the daemon reports whether it is running anything; an idle daemon is
  stopped by the client and a new one started; a busy one is refused with "daemon vX is running
  turns; finish or `hearth daemon stop`".

### Messages

- Snapshot: every `Session`, its `Live` plain data, `Entries` only for loaded sessions, each
  backend's label and capability flags (rewind, unqueue) instead of the `Backend` struct, any model
  lists already fetched, and the current daemon notice.
- Change, per session: `{session, live (without entries), entries: {from: i, items}}`, meaning
  "replace entries from index `i` onwards". This one shape covers appends, streamed `TextDelta`s,
  an `AssistantMessage` replacing the streamed entries, and rewind. Deletion and model listings
  get their own change messages.
- Request: `{id, method, args}`. Reply: `{id, err, result}`. A result is small: the new session ID
  from `NewSession` or `AcceptChip`, a rewind preview, an unqueued message's text, an `IsRepo`
  answer. State never travels in a reply.
- Ordering: the daemon sends every change a request caused **before** that request's reply, so a
  client's local copy is up to date when the call returns or its `done` callback runs.

### Client

- The TUI defines an interface with the `App` methods it calls today (about 30). A client
  implements it with a local copy of the state (`[]*Session`, `map[id]*Live`). Reads (`L`, `Find`,
  `Visible`, `Status`, `Backend`, `Efforts`, `Models`) come from the local copy; commands become
  requests.
- Callback methods (`Unqueue`, `PreviewRewind`) run `done` when the reply arrives.
- `EnsureLoaded` becomes a request; the daemon loads the transcript and broadcasts its entries. No
  per-session subscriptions.
- `Models` stays lazy: the first ask sends a request and reports "loading"; the listing arrives as a
  broadcast change.
- `IsRepo` is a request, because it touches the filesystem and the client may later be remote.
- The TUI's loop keeps its shape: the client's change channel replaces `app.Events()`. The UI
  already keeps the current session as an ID and rebuilds rows every draw, so replacing `*Session`
  values on each change is safe.
- `Notice` is split. Daemon notices (save failures, agent errors, model listing errors, "too late"
  on unqueue) are broadcast to every client. TUI notices stay in the client that raised them.

### Concurrency and backpressure

- Each connection has a reader goroutine feeding the daemon's request channel and a writer goroutine
  draining a buffered outbound queue. When a client's queue is full, the daemon drops that client,
  and the client exits with "disconnected: too slow".

### Permissions

- Pending permissions live in `Live.Perms` and reach every client. The first answer wins; later
  answers get "already answered". Every client's dialog closes when the change removing the
  permission arrives.
- With nobody attached, permissions wait for the next client. No notifications.

### Lifecycle

- There is always a daemon; no in-process mode.
- If `hearth` can't connect, it starts `hearth daemon` detached (`setsid`), with output going to
  `daemon.log` under `HEARTH_HOME`, and waits for the socket.
- The daemon holds a `flock` on `daemon.lock` under `HEARTH_HOME` for its whole lifetime, so only
  one runs per `HEARTH_HOME`.
- The daemon exits after about 30 seconds with no clients attached and nothing running: no running
  turn, no pending permission, no queued message.
- Agents inherit the environment of the client that started the daemon (`PATH`, API keys and the
  backend variables `hearth` reads at start-up).
- `:q` detaches without a warning. If the daemon dies, attached clients exit with "daemon exited,
  see ~/.hearth/daemon.log". No automatic reconnection.

### CLI

- `hearth daemon`: runs in the foreground; autostart uses this.
- `hearth daemon stop`: stops the daemon and every agent, printing what was running.
- `hearth daemon status`: PID, start time, version, client count, running sessions.
- No `restart`; `stop` then `hearth` does the same.

### Commits

1. Protocol, daemon loop and client with its local copy, with tests. Not yet used by `hearth`.
2. TUI behind the interface; `hearth` always connects. Autostart, `flock`, idle exit and the
   version handshake.
3. `hearth daemon stop` and `hearth daemon status`.
4. Roadmap: order becomes daemon → nested sessions → more backends; §3 decisions 1–5 marked as made
   with these answers; the per-session `flock` stopgap paragraph removed.

## Testing Decisions

- Test what a client sees, not how the daemon gets there: drive client methods, then assert on the
  client's local copy and on calls received by the fake agents.
- Main seam: the client interface backed by a daemon running in-process over `net.Pipe`. The `core`
  fakes (store, transcripts, worktrees, launcher, agent) move to a shared test package so the
  daemon tests can reuse them.
- One test per behaviour:
  - two clients see the same change;
  - the first permission answer wins, the second gets "already answered", and both clients' copies
    show it gone;
  - the daemon exits when idle, and does not exit while a turn is running;
  - a version mismatch is refused while a turn is running;
  - after a streamed turn and a rewind, a client's local copy equals the daemon's state.
- The existing TUI tests build the client over an in-process daemon instead of `core.NewApp`; no new
  TUI tests.
- The `core.App` tests are unchanged and remain the guard for `Handle`.
- Autostart, `flock`, `setsid` and the peer-UID check are verified by hand against the real binary.
- Prior art: `testApp` and `withAgent` in the `core` tests, and the TUI's grep-hit test that builds
  an `App` from fakes.

## Open Questions

None left from the planning session.

## Out of Scope

- SSH over a forwarded socket (the protocol must keep it possible).
- Notifications when nobody is attached.
- Automatic reconnection.
- An in-process mode.
- Per-session subscriptions; grep loading every transcript is acceptable for now.
- Token authentication.
- `hearth daemon restart`.
- Nested sessions (roadmap §1), which follow the daemon.

## Further Notes

- The roadmap currently orders nested sessions before the daemon. This spec reverses that, because
  keeping several instances in sync is the first problem expected in practice; commit 4 updates the
  roadmap.
- The roadmap's TUI note "`:q` warns while children are running (until the daemon makes that safe)"
  becomes unnecessary once the daemon lands first.
- Agents inherit whichever environment started the daemon. A `PATH` or API key changed in a later
  terminal won't reach agents until the daemon restarts. `hearth daemon status` showing the start
  time is the only mitigation in this slice.
