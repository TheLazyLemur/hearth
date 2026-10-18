# Nested sessions

> **Status: not ready.** Design notes moved from the roadmap, not an agreed spec. Needs research and
> a planning session before any of it is built.

Parity with Claude Desktop's nested ("side") sessions: a session starts other sessions, they report
back, and the parent is woken when they finish. Desktop does this entirely host-side (in-process MCP
tools, appended prompt text, injected messages); the CLI has no feature for it.

## Core (`core.App`)

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

## Kind policies

| Kind | Started by | On child turn end | Wakes parent | Roster |
|---|---|---|---|---|
| `side` | the model (`start_session`) | note | yes | yes |
| `fork` | the user, or `hand_off` | note | no, waits for the parent's next turn | yes |
| `btw` | the user (side chat) | nothing | no | hidden; archived on close once settled |
| `task` | the user accepts a chip | nothing | no | yes |

## Tools (`core.Tools`, so every backend gets them)

- `start_session`: `initiation` `user_asked` first; `context` fresh (prompt plus background) or fork;
  `use_worktree`, `model`, `effort`, `permission_mode`, `name`, `wake_when_done`. Children default
  to the parent's backend; a `backend` argument would let one agent start another kind.
- `send_message`: delivered to the target as a queued user turn, wrapped with the sender's id and name.
- `list_sessions {linked}`, `get_session`.
- Guidance lives in tool descriptions and in a header `Spawn` puts on the child's brief (who started
  it, that its last message is relayed, message the parent only when blocked). Our own wording, not
  Desktop's.

## TUI

- Children nested under their parent in the sidebar, with state.

## Harness seams

- System-prompt hook, built by core per session and delivered by each adapter: Claude
  `--append-system-prompt` on every launch including resume; pi through `host.mjs`
  `before_agent_start`. Deferred: tool descriptions carry the guidance until they prove too weak.
  Keep the system prompt static; anything that changes goes through the reminder queue.
- `Fork` capability on `Backend`, beside `Rewind` and `Unqueue`. Claude: `--resume --fork-session`
  (unverified over stream-json). pi: unknown.
- Tool calls already know their caller: `CallTool` returns an event and `Handle` routes it by
  `AgentEvent.SessionID`. New tools follow the `TaskSuggested` pattern.

## Later variants on the same core

- `btw` side chats, user fork, `hand_off_to_session`.
- `offer` cards ("New session" / "Do it here", blocking the tool call) and `own_initiative` proposal
  cards that expire after a week.
- `detach_session`, `stop_session`, `archive_session`.
- Placement (`cwd`, SSH host, presets, cloud) only once hearth has those features underneath.
