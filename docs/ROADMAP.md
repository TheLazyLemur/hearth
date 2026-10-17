# Roadmap

hearth is a harness on top of coding agents, not a control panel over their terminals. It drives
each agent through a structured protocol, owns sessions, worktrees, permissions, tools and
orchestration itself, and treats a backend as little more than a model loop.

Order: **nested sessions → daemon → more backends**. Everything new lives in `core.App` as plain
data, never in `tui`, so it moves behind the daemon's socket unchanged.

## 1. Nested sessions

Parity with Claude Desktop's nested ("side") sessions: a session starts other sessions, they report
back, and the parent is woken when they finish. Desktop does this entirely host-side (in-process MCP
tools, appended prompt text, injected messages); the CLI has no feature for it.

### Core (`core.App`)

- Widen `SpawnedFrom`: `Name` (short handle), `Kind` (`side` | `fork` | `btw` | `task`), `Detached`.
- `App.Spawn(parent, spec)`: fresh or fork, own worktree or shared checkout, inheriting the parent's
  backend, model, effort and permission mode (never higher). `AcceptChip` becomes `Spawn` with
  Kind `task`.
- Reminder queue per session: pending text sent as `<system-reminder>` ahead of the next user
  message. The transcript view hides these blocks, live and from JSONL.
- Turn-end router in `Handle`: a child's result becomes a note to its parent per its Kind:
  title, how it ended (finished, error, stopped, lost), branch and PR, first 400 characters of its
  report. Notes queue on the parent (cap 32).
- Roster reminder ("Linked sessions now"): every family member's name, id and state (working, idle,
  failed, needs the user, done with PR), refreshed for the whole family on change.
- Wake: once no child is working and the parent is idle (no turn, no pending permission or
  question), send the parent a `<side-sessions-event>` turn. Capped per user message from day one.
- Persist lineage, queued notes and wake state, so a restart doesn't drop what a parent is owed.

### Kind policies

| Kind | Started by | On child turn end | Wakes parent | Roster |
|---|---|---|---|---|
| `side` | the model (`start_session`) | note | yes | yes |
| `fork` | the user, or `hand_off` | note | no, waits for the parent's next turn | yes |
| `btw` | the user (side chat) | nothing | no | hidden; archived on close once settled |
| `task` | the user accepts a chip | nothing | no | yes |

### Tools (`core.Tools`, so every backend gets them)

- `start_session`: `initiation` `user_asked` first; `context` fresh (prompt plus background) or fork;
  `use_worktree`, `model`, `effort`, `permission_mode`, `name`, `wake_when_done`.
- `send_message`: delivered to the target as a queued user turn, wrapped with the sender's id and name.
- `list_sessions {linked}`, `get_session`.
- Guidance lives in tool descriptions and in a header `Spawn` puts on the child's brief (who started
  it, that its last message is relayed, message the parent only when blocked). Our own wording, not
  Desktop's.

### TUI

- Children nested under their parent in the sidebar, with state.
- `:q` warns while children are running (until the daemon makes that safe).

### Later variants on the same core

- `btw` side chats, user fork, `hand_off_to_session`.
- `offer` cards ("New session" / "Do it here", blocking the tool call) and `own_initiative` proposal
  cards that expire after a week.
- `detach_session`, `stop_session`, `archive_session`.
- Placement (`cwd`, SSH host, presets, cloud) only once hearth has those features underneath.

## 2. Harness seams

- System-prompt hook, built by core per session and delivered by each adapter: Claude
  `--append-system-prompt` on every launch including resume; pi through `host.mjs`
  `before_agent_start`. Deferred: tool descriptions carry the guidance until they prove too weak.
  Keep the system prompt static; anything that changes goes through the reminder queue.
- `Fork` capability on `Backend`, beside `Rewind` and `Unqueue`. Claude: `--resume --fork-session`
  (unverified over stream-json). pi: unknown.
- Tool calls already know their caller: `CallTool` returns an event and `Handle` routes it by
  `AgentEvent.SessionID`. New tools follow the `TaskSuggested` pattern.

## 3. Daemon

`App`, the agent adapters and later our own tools run in a background process that owns the agent
processes and the store, listening on a Unix socket. The TUI becomes a client: snapshot, then a
stream of changes. One binary: `hearth daemon`; plain `hearth` starts it if needed, then connects.

Requirements:

- Running turns, nested children and parent wakes survive `:q` and the laptop lid.
- Every open hearth instance sees the same live state.
- SSH via the same protocol over a forwarded socket.
- Opening hearth in any terminal stays the natural way in. Sessions are shared; view state is per
  client: focus, selection, collapsed groups, scroll, visual mode, and the launch folder. A client
  launched in a folder focuses that project's group and selects its latest session; if the project
  has no sessions, it shows the group with "new session here" instead of an empty list.

Decide before building:

1. Lifecycle: run forever, or exit when no client is attached and nothing runs?
2. Nobody attached: permission requests wait, notify, or follow a per-session rule?
3. Several clients answer the same permission: which wins?
4. Upgrades: a new binary meets an old daemon mid-turn. Default: refuse and say why.
5. State format: split session state into plain data and live handles (the bulk of the refactor).

Until then, several instances are unsafe: session JSON is last-writer-wins and two instances can
resume the same CLI session. Possible stopgap: a per-session `flock` in `~/.hearth/sessions`, with
other instances showing the session read-only.

## 4. Backends

Contract every backend must meet:

- start, resume, interrupt; messages sent mid-turn are queued;
- structured events: turn start and end, assistant text, tool calls and results, errors;
- serves `core.Tools`, and gates every tool that isn't ours through hearth's permission flow;
- accepts appended system-prompt text (see §2).

Optional, advertised as capabilities: fork, rewind, unqueue, native permission modes, model and
effort listing. Keep these as bools until a third backend shows the right shape.

End state: built-in tools off, hearth's own tools in their place; a backend is a model loop plus
our tools behind our gate. Children default to the parent's backend; a `backend` argument on
`start_session` would let one agent start another kind.

## 5. Tracking Claude Desktop

Desktop's app bundle is readable JavaScript (`npx @electron/asar extract app.asar`). Following a day
or two behind is the plan. Possible automation: extract each release and diff its tool definitions,
prompt text and control-protocol subtypes against the last one.

## Deferred until it hurts

- Transcript grep loads every transcript on the UI goroutine; prefilter with ripgrep and parse only
  the hits, off the UI goroutine.
- Fuzzy matching is greedy; the markdown cache clears wholesale at 500 entries.
