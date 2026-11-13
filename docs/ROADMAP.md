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
  notifications" decision. Reuse baton's notifier: ntfy with one answer button per option,
  128-bit prompt ids, and "already answered" for a second tap.
- SSH over a forwarded socket, or §5's network listener.

### 2. Nested sessions

A session starts other sessions, they report back, and the parent wakes when they finish:
`start_session`, `send_message`, `list_sessions`, `get_session`, and children nested under their
parent in the sidebar. Needs research first; draft notes in `docs/specs/nested-sessions.md`.
Session IDs must name their machine, so messages can cross machines (§5).

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

- **Barrier at hearth's tool gate** (from baton, §5): before a tool that changes state runs, a
  checkpoint of the session (transcript plus working tree) is committed and replicated to a
  peer. Because hearth owns the gate, this holds for every backend, not only through Claude
  Code's hook.

### 5. Multi-machine, by consensus (no hub)

A daemon on each machine, all equal; no hub to go down. This is baton
(`~/Programming/tools/baton`, built and tested live), ported in as hearth's cluster layer.
Decision: `docs/decisions/0001-consensus-instead-of-a-hub.md`. Builds on §1's remote attach and
§2's messaging.

- **Paths: answered (baton's spike S17 and live test S18, ADR-0026).** A session moves between
  machines whose project paths differ: the new owner writes the transcript into its own project
  folder, rewrites each row's `cwd` (metadata only, never message content) and appends a notice
  with the new paths. The first call after the move read the whole conversation from cache
  (44,216 read, 1,347 written). So no shared absolute root: each machine keeps its own (baton's
  default `~/.baton/workspace`), which opens the way to sessions in the user's own folders.
  Restore also fetches the newest checkpoint from the peers, or a machine that lost its repo
  resumes from an older one (found in S18). Still to measure: large saved tool outputs and a
  long real session.
- **Cluster.** Embedded etcd on three voters (two places at least), mutual TLS between daemons
  with the cluster's own CA, `init` / `invite` / `join` with single-use join tokens (baton's
  ADR-0025). Clients and the web page keep hearth's token pairing.
- **Ownership.** Each session has one owner per term: a create-if-absent key on a 15 s lease,
  term-checked writes, a self-fence at 9 s without renewal, and a fence that denies tool calls
  everywhere else. baton's invariants I1–I5 and their tests come with it.
- **Takeover and moves.** Checkpoints replicate to peers before etcd points at them. A dead
  owner's sessions are taken over by rank, resumed from the newest checkpoint, with a notice of
  the tool call that may have half run. A planned move waits for the turn to end, including an
  open question.
- **Cross-machine routing.** Any daemon forwards an operation or a stream to the session's
  owner. Sessions gain a machine; a new session goes to the machine it names, and the folder
  picker asks that machine.
- **Memory and config sync.** Each machine publishes its copy as a ref of its own and pushes it
  to its peers; the newest file wins and nothing is lost. Deletions need markers (baton's
  known gap).
- **Credential broker.** Still needed (a GitHub App key minting tokens scoped to a job's repos,
  a Bitbucket OAuth consumer), but without a hub it needs a new home. Open question below.

- **Known issues to fix while porting,** not copy (details, causes and fixes in baton's
  `docs/known-issues.md`):
  1. Git-ignored files (`.env`, local config, test data) never travel: needs a carry list.
  2. Synced memory and config files can't be deleted: needs tombstones.
  3. A pending question is lost on an immediate move or crash takeover, and leaves a stale prompt.
  4. A terminal agent left running after its attach process dies is unfenced.
  5. Session commands fail for ~15 s after a daemon restart instead of waiting.
  6. A move waiting for an answer gives no sign it's waiting.
  7. baton's docs wrongly require one Claude account on every worker.
  8. A subagent with its own permissions can do what a denied call wanted (hearth's tool gate
     should cover it).
  9. Newest-wins sync trusts each machine's clock.

Later, once the above works:

- Run a command on one machine or all of them.
- Expose a worker's dev server as a hostname, through whatever fronts it.

### 6. Accounts

Each session is bound to the account chosen for it, and keeps it when it moves, so a move never
loses the prompt cache. No automatic rotation between accounts. Measurements and options in
baton's `docs/wishlist/accounts.md`.

### 7. Publish

MIT licence, README (problem, invariants, a recorded kill-and-takeover demo), CI, personal
details removed, a secret scan, a fresh git history. baton's spec, ADRs and recorded test
evidence come along as design material.

## Open questions

- **Memory conflicts.** baton merges automatically (newest file wins, `MEMORY.md` lines
  unioned). The hub plan had an agent merge conflicts and show only contradictions. Keep
  automatic merging, or add the agent merge on top?
- **Credential broker without a hub.** Secrets on each machine, or encrypted to each worker's
  key and replicated through the cluster?

## Deferred until it hurts

- Transcript grep loads every transcript on the UI goroutine; prefilter with ripgrep and parse only
  the hits, off the UI goroutine.
- Fuzzy matching is greedy; the markdown cache clears wholesale at 500 entries.
