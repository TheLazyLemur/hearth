# Claude backend as a Claude Code mod

> **Status: contingency.** Not scheduled. Build it only if `claude -p` / stream-json stops being
> usable for hearth (restricted on subscriptions, removed, or rate-limited apart from interactive
> use). Until then `internal/cli` stays as it is: its protocol is the one the Agent SDK uses, and
> the mod API is early access and changes between releases.

Run each Claude session as an interactive `claude` in a pty that hearth owns, loaded with a `hearth`
mod (`--plugin-dir`) that carries hearth's tools, permission gate, prompt text and events over a
bridge to the daemon. The model loop is Claude Code's; sessions, worktrees, permissions, tools and
orchestration stay in `core.App`, as the roadmap's Backends goal asks.

Subagents don't change: `start_agent` already starts a hearth session, which is one more `claude`
process launched the same way. Claude Code's own Agent tool stays off.

## Shape

```
daemon ── hearth bridge <session> ── stdout lines ──▶ mod   (hearth → Claude)
       ◀── per-session Unix socket ◀── POST ──────── mod   (Claude → hearth)
daemon ── pty ── claude --plugin-dir <mod> [--resume <id>]   (output discarded)
```

- **Process.** The daemon starts `claude` in a pty, sets a window size, and discards its output
  (`go io.Copy(io.Discard, ptmx)`): nothing reads the screen, but an unread pty fills and blocks the
  process, hooks included. hearth keeps owning start, kill and exit status, as today. Needs a pty
  package (`creack/pty`); `go.mod` has none. `claude --bg` also works and adds `claude attach` for
  free, but hands the process's lifecycle to Claude Code.
- **Session identity.** The launch sets `HEARTH_SESSION_ID`; the mod does nothing when it is unset,
  so it is inert in an ordinary `claude` session.
- **hearth → Claude.** In `session.start` the mod runs `hearth bridge <session>` with
  `$.process.spawn` and reads one JSON command per stdout line, the pattern of
  `~/.claude/mods/web-bridge`. `$.process.spawn` writes stdin once and closes it, so replies don't
  go back that way.
- **Claude → hearth.** Fire-and-forget events go by `$.http.fetch(url, { socketPath })` to a socket
  the bridge listens on, queued in order. Anything that waits on the person goes through a spawned
  child instead (below).
- **The daemon's protocol stays NDJSON.** The bridge is a daemon client with an agent role; the
  daemon needs no HTTP endpoint.

## Mapping `core.Agent`

| hearth needs | stream-json today | mod |
|---|---|---|
| hearth's tools | in-process MCP server `hearth` | `$.tool.register`; a plugin named `hearth` keeps the names `mcp__hearth__spawn_task` etc. |
| permission gate | `can_use_tool` control request | `tool.call` hook (runs above Claude Code's own check): ask hearth, then `{ deny }` or `next(e)`, with a `.catch` that denies |
| appended system prompt | — | `prompt.compose`, a section with `scope: 'session'` |
| `Initialized` | `system/init` | `session.start`: `$.session.id()`, `model()`, `version()` |
| `TextDelta` | `stream_event` deltas | `turn.step` chunks, posted as they arrive |
| `AssistantMessage`, `UserMessage` | `assistant`, `user` lines | `session.append` (every kept row, main loop where `agentId` is unset) |
| `TurnEnded` | `result` | `turn.complete` |
| `Compacted` | `compact_boundary` | `session.append` with door `compaction` |
| `SendUser` while idle | `user` line | `$.prompt.submit({ text, asUser: true })` |
| `SendUser` mid-turn | CLI queues, echoes `isReplay` | hearth holds the queue and hands the next message over at a step boundary with `$.session.append` (a hidden user row) |
| `Interrupt` | `interrupt` | `$.turn.abort({ turnId })`, the id from `turn.start` |
| resume | `--resume` | same flag |
| model, effort, permission mode | `--model`, `--effort`, `--permission-mode` at launch; control requests after | same flags at launch; `$.config.set` or `$.command.run` after (keys not checked) |

## Gaps and how hearth fills them

- **Unqueue.** No `cancel_async_message`. hearth holds queued messages itself, so unqueueing drops
  one before it is handed over. `Consumed` becomes hearth's own record of the handover.
- **Titles.** No `generate_session_title`. The mod calls `$.model.complete`, or hearth makes its own
  model call.
- **Model list.** No `initialize` reply. hearth keeps its own list for Claude, or reads the model
  row of `$.config.list()` (whether that row carries the choices is not checked).
- **Rewind, files.** No `rewind_files`. hearth snapshots the worktree with git at each user message.
- **Rewind, conversation.** No API. hearth truncates Claude Code's JSONL transcript and resumes from
  the cut. Fragile: hearth doesn't own that format.
- **Workspace trust.** Interactive sessions (pty and `--bg` alike) need each folder trusted, and
  every hearth worktree is a new folder; `-p` and SDK sessions skip the check, which is why
  `~/.claude.json` holds no hearth paths today. Two ways in, both on Claude Code internals:
  - write `projects[<path>].hasTrustDialogAccepted` into `~/.claude.json` before launch. It races
    with running `claude` processes that write the same file, and it is not yet checked that it
    lets the session start;
  - answer the "Quick safety check" dialog in the pty by sending Enter when it shows. Screen
    scraping, breaks when the wording changes.

## Spike findings (Claude Code 2.1.303, 2026-10-19)

A throwaway mod gated Bash on a bridge that held its answer for 200 s.

- **`$.http.fetch` can't wait on the person.** The host aborts it at 30 s ("no complete answer
  within 30000ms"). Use it for events only.
- **`$.process.spawn` can.** A `tool.call` hook held a spawned `curl --unix-socket` for 200 s, got
  "allow" and the tool ran. The permission gate goes this way.
- **`claude --bg` refuses an untrusted folder** at once ("Workspace not trusted"), including a
  fresh worktree under a trusted repo's `.claude/worktrees/`.
- **A pty-launched `claude` stops at the trust dialog** in a fresh folder.
- The wait tests ran under `-p` (mods load there too), because `--bg` would not start.

## Open questions

- Does writing the trust flag let a pty or `--bg` session start without the dialog?
- Which `$.config.set` keys change model, effort and permission mode mid-session?
- Do `turn.step` chunks match today's `TextDelta` granularity?
- Does a hidden user row from `$.session.append` mid-turn read to the model like a queued prompt?
- Does a spawned waiter survive the hook's budget for an hour-long permission wait, or only minutes?
