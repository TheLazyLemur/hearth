# ACP backend

The agent half of this design is grimalkin's `docs/specs/acp.md`.

## Problem Statement

Each hearth backend speaks a private protocol: Claude Code's stream-json and
pi's RPC mode. Every new agent means a new adapter. grimalkin, an agent with
its own providers and models, can't run in hearth at all.

## Solution

A generic backend for the Agent Client Protocol (ACP). hearth spawns an ACP
agent per session and drives it over stdio. grimalkin, through `grimalkin acp`,
is the first agent. Another ACP agent is another registration, not another
adapter.

What stable ACP lacks, hearth gets from extensions it uses only when the agent
advertises them:

- **Steering** through `_session/steering`, which claude-agent-acp and codex-acp
  already ship. Without it, hearth queues until the turn ends.
- **hearth's tools** over MCP-over-ACP, a draft RFD. The agent sends MCP
  requests up the ACP pipe and hearth answers them with the MCP code that
  serves Claude, so no subprocess is needed.
- **Appended system-prompt text** as `_meta.systemPrompt.append` on
  `session/new`.

For the user, a grimalkin session behaves like a Claude session: text streams,
tool calls stop at hearth's gate, mid-turn messages steer, `start_agent` makes
tabs, old sessions reopen, and the model and effort come from grimalkin's
config.

## User Stories

1. As a hearth user, I want to start a session on the grimalkin backend, so that I can use grimalkin's providers and models in hearth.
2. As a hearth user, I want grimalkin's reply to stream into the transcript, so that it feels like a Claude session.
3. As a hearth user, I want every grimalkin tool call to stop at hearth's permission gate, so that it can't run what I haven't allowed.
4. As a hearth user, I want my session's permission mode to apply to grimalkin's tool calls, so that a mode means the same on every backend.
5. As a hearth user, I want `start_agent`, `spawn_task` and `dismiss_task` to work from a grimalkin session without a prompt, so that orchestration works as on Claude.
6. As a hearth user, I want a message typed mid-turn to reach grimalkin at the next tool-round boundary, so that I can steer without cancelling.
7. As a hearth user, I want a queued message to stay pending until the agent has put it into the conversation, so that I'm never told a lost message was delivered.
8. As a hearth user, I want a message typed while the agent is idle to start a normal turn, so that steering never swallows a message.
9. As a hearth user, I want interrupt to stop a turn, so that I can cancel runaway work.
10. As a hearth user, I want to reopen an old grimalkin session with its history, so that I can pick up where I left off.
11. As a hearth user, I want to pick and switch a grimalkin session's model and effort, so that I can trade cost for capability.
12. As a hearth user, I want `start_agent` to list grimalkin's models and efforts, so that a subagent can run on one.
13. As a hearth user, I want an agent failure or crash to end the turn with an error, so that a session never hangs.
14. As a hearth developer, I want the backend generic over ACP agents, so that adding one is registration.
15. As a hearth developer, I want an agent without steering to still work by queueing to turn end, so that less capable agents degrade cleanly.
16. As a hearth developer, I want hearth's tools served by the same MCP code as Claude's, so that tools can't drift between backends.

## Implementation Decisions

- **A new `Launcher`, registered per agent.** Each entry names the agent's
  command; grimalkin's is `grimalkin acp`. The client side is built on
  `github.com/coder/acp-go-sdk`.
- **Start.**
  - Spawn the agent, then `initialize`, then `session/new` with:
    - `cwd`
    - `mcpServers: [{type: "acp", name: "hearth", id}]`, with a per-session
      `id`
    - `_meta.systemPrompt: {append}` when hearth has text to append. This is
      claude-agent-acp's own key and shape.
  - The returned `sessionId` is stored as the session's `CLISessionID`.
- **hearth's tools.**
  - The wire is the MCP-over-ACP draft acp-go-sdk v0.13.5 implements. The
    agent's `mcp/connect` names hearth's server `id` and gets a
    `connectionId`. `mcp/message` on that connection carries the MCP
    requests.
  - The SDK never dispatches an inbound `mcp/message` on the client side, so
    hearth intercepts it on the connection.
  - The requests are answered by the MCP code that serves Claude: `tools/list`
    returns `core.Tools`, and `tools/call` goes to `core.CallTool`.
  - The server `id` identifies the session, so no socket or token is needed.
  - We move to the current stateless RFD (`serverId` and `requestId`) when the
    SDK does.
  - Rejected for now: a stdio bridge subprocess (`hearth mcp`). It comes with
    the first agent that lacks MCP-over-ACP.
- **Events.**
  - `agent_message_chunk` becomes `TextDelta`, and assembled messages become
    `AssistantMessage` entries.
  - `tool_call` and `tool_call_update` become tool entries.
  - The prompt's `stopReason` becomes `TurnEnded`, and process exit becomes
    `Exited`.
- **Permissions.**
  - `session/request_permission` becomes `PermissionRequested`, answered
    through the session's mode by `permit()`, as for pi.
  - hearth's own tools are allowed without asking, as
    `--allowedTools mcp__hearth__*` does for Claude.
  - The agent applies no modes.
- **SendUser.**
  - During a turn, with `_meta.steering.supported` advertised, send
    `_session/steering` with `{sessionId, prompt}` and
    `_meta.steering.idleBehavior: "promptRequired"`. This is the shape
    claude-agent-acp and codex-acp ship.
  - `Consumed{UUID}` is emitted on `{outcome: "injected"}`. No agent echoes a
    steered message, and grimalkin guarantees an injected message reaches the
    conversation even if the turn is cancelled.
  - On `startedNewTurn` (codex-acp ignores the idle opt-in), the steer has
    started a turn: emit `Consumed` and treat the session as running.
  - On `promptRequired`, `failed`, or with no steering, hold the message and
    send it as the next `session/prompt` when the turn ends; `Consumed` follows
    when it is sent.
- **Interrupt** sends `session/cancel`.
- **SetModel and SetEffort** send `session/set_config_option` on `model` and
  `thought_level`.
- **Models.** The launcher's listing starts a throwaway agent, as the Claude
  backend does, and reads `configOptions` from a `session/new`.
- **Capabilities.** `Rewind: false`, `Unqueue: false`.
  - Rejected: an unsteer extension. Steering delivers within a tool round, so
    there's little window to pull a message back.
- **Rename and GenerateTitle** are handled by hearth and answered with a
  synthetic `Reply`, as pi's backend does.
- **Reopening a session.**
  - Opening a session that isn't running starts the agent and calls
    `session/load`; the replayed updates become its entries, and the agent
    stays running.
  - Rejected: reading grimalkin's JSONL, which ties the backend to one agent.
  - Rejected: hearth storing its own transcript copy, a new storage job.
- **Order,** shared with grimalkin and demoed end to end with the real
  `grimalkin acp`:
  1. One text turn.
  2. Resume through `session/load`. hearth drops processes on shutdown, crash
     and delete, so this comes before anything else.
  3. Tools, the gate and hearth's tools.
  4. Steering.
  5. Models and efforts.

## Testing Decisions

- **One seam: `Launcher.Start` → `Agent` and its events channel,** against a
  fake ACP agent on acp-go-sdk's agent side over pipes. Tests check the
  `core.Event`s produced and the ACP messages sent.
- **Cases:**
  - Chunks become `TextDelta`, and the stop reason becomes `TurnEnded`.
  - `PermissionRequested` is answered by `Allow` and `Deny`, and hearth's tools
    are auto-allowed.
  - `tools/list` and `tools/call` over `mcp/message` reach `core.CallTool`.
  - A mid-turn `SendUser` steers and emits `Consumed` on `injected`.
  - `promptRequired`, or an agent without steering, queues until turn end.
  - `Interrupt` sends `session/cancel`.
  - `SetModel` and `SetEffort` send `set_config_option`.
  - The model listing reads `configOptions`.
  - Reopening a session replays through `session/load`.
- **Prior art:** the Claude backend's tests, where stream lines decode to core
  events and a `spawn_task` call is answered over MCP.

## Open Questions

- Should a model listing reuse a running session's `configOptions` rather than
  a throwaway agent?

## Out of Scope

- The stdio MCP bridge.
- Moving the Claude backend onto claude-agent-acp, though this backend would
  allow it and would cover `claude-mod-backend.md`'s contingency.
- Unqueue, rewind and fork on ACP agents.
- Showing an agent's own subagents (the draft ACP subagents RFD).

## Further Notes

- The backend is generic in shape, but grimalkin is the only agent advertising
  MCP-over-ACP at first, so it effectively requires grimalkin until the bridge
  exists.
- This is the third backend shape the roadmap waited for before turning the
  optional capability bools into something firmer.
