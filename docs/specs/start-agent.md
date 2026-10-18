# start_agent

Two parts: choosing an agent's backend, model and effort (shipped in #3), and showing agents as tabs.

## Part 1: any backend and model

### Problem Statement

The `start_agent` tool from PR #2 can only start an agent on its parent's backend and model. A Claude session can't hand a broad search to a cheaper pi model, and a pi session can't hand a hard task to Opus. Effort has a quieter problem. Each model supports its own effort levels, but hearth saves effort on the session. When a model gets an effort level it doesn't support, neither backend reports an error. Claude lowers the level. pi raises it to the next level the model supports, which can cost more. hearth's pi adapter makes this worse: it offers every pi reasoning model the same six levels, whatever the model really supports.

### Solution

The parent's model can start an agent on any backend and model that hearth has listed. It can also set the agent's effort. hearth lists every backend's models at startup, and each session's `start_agent` description lists the models the agent can pick from, with each model's effort levels. With no choice, the agent uses its parent's backend and model, as it does now. hearth refuses a backend, model or effort level it can't honour, and says what is valid. It never lets a backend quietly change the value. The agent always keeps its parent's permission mode. hearth lists each pi model's real effort levels, which also corrects the `:effort` picker.

### User Stories

1. As a user, I want my session's model to start an agent on a cheaper model for broad searches, so that routine work costs less.
2. As a user, I want my session's model to start an agent on a stronger model for a hard sub-task, so that difficult work gets the right model.
3. As a user, I want a Claude session to start a pi agent and the other way round, so that I'm not tied to one provider per task.
4. As a model in a hearth session, I want `start_agent`'s description to list every backend and model I can choose, so that I can pick without guessing IDs.
5. As a model, I want each listed model to show its effort levels, so that I only ask for a level it supports.
6. As a model, I want Claude's aliases (`opus`, `sonnet`, `haiku`, `default`) in the list, so that I can use the names people use.
7. As a model, I want to leave out backend and model and get my own, so that the usual case needs no extra parameters.
8. As a model, I want to give only a backend and get that backend's default model, so that I don't need to know model IDs to switch provider.
9. As a model, I want to leave out effort and keep my own effort when the target model supports it, so that agents work as hard as I do by default.
10. As a model, I want an agent on a model without my effort level to use that model's default, so that the agent still starts.
11. As a model, I want an effort level the target model doesn't support refused, with that model's valid levels in the error, so that I can retry correctly.
12. As a model, I want an unknown backend or model refused, with the valid choices in the error, so that I can correct the call in one retry.
13. As a model, I want a clear refusal while model lists are still loading, telling me to leave backend and model out or retry, so that I'm not left waiting.
14. As a user, I want hearth to refuse values a backend would quietly change, so that I never pay for a higher effort level than the one requested.
15. As a user, I want every agent to keep its parent's permission mode, whatever backend it runs on, so that starting an agent never widens what can run without asking.
16. As a user, I want the session list to show each agent's backend, so that I can see where an agent runs and what it spends.
17. As a user, I want the `:effort` picker for a pi model to show only the levels that model supports, so that the level I choose is the level it uses.
18. As a user, I want pi models that support `max` to offer `max`, so that I can use their highest level.
19. As a user, I want pi models without reasoning to offer no effort levels, so that I don't set a value pi ignores.
20. As a user, I want the agent's backend, model and effort saved on its session, so that I can see them and they survive a restart.
21. As a user, I want an agent on another backend to report back the same way as one on the same backend, so that the parent's model doesn't need to know where the agent ran.
22. As a user, I want nested agents to choose from the same list, so that an agent can start its own agents on any backend too.
23. As a user, I want the depth and concurrency limits to apply whatever the backend, so that agents on another provider can't run past them.

### Implementation Decisions

- **New optional parameters.** `start_agent` gets three optional parameters: `backend`, `model` and `effort`. They are separate, and each maps straight onto the agent session's backend, model and effort fields. There is no combined `backend:model` ID.
- **Defaults.**
  - With `backend` and `model` both left out, the agent uses the parent's backend and model.
  - With `backend` alone, the agent uses that backend's default model (no model set), the same as a new session.
  - With `effort` left out, the agent keeps the parent's effort if the target model lists that level. Otherwise it has no effort set and uses the model's default.
- **Permission mode is not a parameter.** The agent always gets exactly the parent's mode. hearth offers only `default`, `acceptEdits`, `plan` and `auto`, and both backends handle all four.
- **Checks happen in core, at once.** `start_agent` checks the requested values against the model lists hearth already has. It does not wait for a list to arrive, so `Handle` stays synchronous. It refuses with a coded error:
  - for an unknown backend, listing the valid backends;
  - for a model that isn't on the backend's list, listing that backend's models;
  - for an effort level the target model doesn't list, listing its levels;
  - when the lists are still loading: "model lists are still loading; leave backend and model out or retry shortly".
- **No backend clamping.** hearth never passes on a value the backend would quietly change. Claude lowers an unsupported level and pi raises it to the next one it supports, so both are refused before launch.
- **Lists load at startup.** hearth starts listing every backend's models when it starts, instead of waiting for the first time the TUI asks for them.
- **The description is built at each launch.** The `start_agent` description becomes a per-launch value built from the lists hearth has when the session launches. Each entry shows backend, model ID, name and effort levels. Claude's aliases stay in; descriptions are left out to keep it short. A session launched before the lists arrive gets a description without them. A launched session's description doesn't change if the lists change later.
- **The launcher interface changes.** `Launcher.Start` receives the tool definitions for this launch instead of each adapter reading the static global. The pi adapter sends them to the host at launch. The Claude adapter answers MCP `tools/list` with them.
- **pi effort levels follow pi's own rule.** The pi adapter computes each model's effort levels the way pi-ai's `getSupportedThinkingLevels` does:
  - a model without reasoning gets no levels: hearth's "no effort setting" (pi-ai itself reports `off` only);
  - otherwise each of `off, minimal, low, medium, high, xhigh, max` is kept unless the model's `thinkingLevelMap` sets it to `null`;
  - `xhigh` and `max` are kept only when the map gives them a value.

  `get_available_models` already returns `reasoning` and `thinkingLevelMap`, so no extra pi call is needed. pi's level list gains `max`.
- **Reports, limits and lineage don't change.** Agents on another backend use the same session link, `<agent-report>` messages, depth cap (3) and concurrency cap (20) as today.

### Testing Decisions

- Tests check behaviour through public seams, never internals. A good test starts an agent through a `start_agent` call and checks the session that results, the refusal text, or the tool definitions a launch receives.
- **`core.App` seam.** These tests drive `Handle` with the fake launchers and the fake model lists. They cover:
  - inheriting, with no parameters;
  - backend alone giving that backend's default model;
  - an explicit model on another backend;
  - effort carried over when the target model supports it, and dropped when it doesn't;
  - refusals for an unknown backend, an unknown model, an unsupported effort and lists still loading;
  - the listed models appearing in the tool definitions a launch receives (the fake launcher records them).

  Prior art: the `start_agent` tests from PR #2 in the core app tests.
- **pi model-listing seam.** Extend the existing test of pi's available models so that it covers a model with a `thinkingLevelMap` that leaves some levels out, a model with no map, and a model without reasoning.
- **Adapter wiring** that passes the tool definitions on to pi and to Claude's MCP `tools/list` is covered by the existing MCP and host tool-call tests. It gets no new test.

### Settled Questions

- **Failed listing.** A backend whose listing fails is left out of the description. A `start_agent` call that names it is refused with the listing error, for example "pi is not available: listing models: exec: pi not found". The existing rule stays: a failed listing is cached as empty until restart. Inheriting the parent's backend skips the check, so it still works.
- **Startup cost.** hearth lists every backend at startup. With the current setup, this is about 4 s of background work (Claude 3.3 s, pi 0.6 s) and two short-lived processes, and the UI doesn't wait. Claude's listing writes no session.
- **Long lists.** No cap. The current 13 models add about 800 characters. A `shortcut:` comment records when to revisit: past about 50 models or 4 kB.
- **Showing where an agent runs.** `start_agent`'s success reply names the backend, model and effort that hearth chose. The `↳ agent` marker and the report header don't change, because the session row's backend tag already shows the backend to the user.

### Out of Scope

- Named profiles that the user sets up (for example `fast` and `deep`).
- A switch to turn off agents on another backend. The cost is the user's call, and the backend tag on the session row shows it.
- A `permission_mode` parameter, or any agent mode other than the parent's.
- A separate worktree for each agent.
- Updating an already-launched session's description when the model lists change.
- A `list_models` tool.
- Letting a backend clamp effort instead of hearth checking it.

### Further Notes

- **Claude Code 2.1.302 effort behaviour**, observed through a proxy that captured its API requests:
  - an unsupported level is lowered without notice (`xhigh` on `claude-sonnet-4-6` is sent as `high`);
  - a model without an effort setting (`claude-haiku-4-5`) drops it;
  - a word that isn't a level gives a stderr warning and the default effort.

  None of these is an error.
- **pi's `clampThinkingLevel`** searches upward first: on Kimi K3, `medium` becomes `high` and `xhigh` becomes `max`. A word that isn't a level becomes the model's first supported level.
- **Conflicts with the current code:**
  - `start_agent`'s description is static today, and every adapter reads the global tool list.
  - pi models currently get the same six levels and no `max`.
  - hearth lists models only when the TUI first asks.

## Part 2: agent tabs

### Problem Statement

Every agent is a top-level row in the sidebar, and nothing there links it to the session that started it. A few agents bury the sessions the user started, and finished agents never leave the list. Inside the parent, an agent shows up only as the `↳ agent` line once its report arrives. To watch an agent at work, the user has to find its row among the others.

### Solution

Agents open as vim-style tabs of the session the user started. The tab line sits above the transcript and is shown only while that session has agent tabs open. Agents leave the sidebar. The session's row shows how many of its agents are working, and a "needs you" mark when one waits on a permission or a question. Tabs close by themselves, following the rules Claude Code 2.1.302 uses for its subagent panel. A closed agent's session and transcript stay, and the user can reopen it from the parent's `↳ agent` line.

### User Stories

1. As a user, I want agents left out of the sidebar, so that it lists only the sessions I started.
2. As a user, I want a working agent to appear as a tab on the session that started it, so that I can watch it without hunting for it.
3. As a user, I want each tab to show its number, title and state (working, done, failed, needs you), so that I can see every agent's progress at a glance.
4. As a user, I want a tab and its session's sidebar row marked when an agent waits on a permission or a question, so that a blocked agent is never stuck out of sight.
5. As a user, I want the sidebar row of a session to show how many of its agents are working, so that I know it is busy without opening it.
6. As a user, I want `gt`, `gT` and `{n}gt` to move between tabs, so that switching works the way it does in vim.
7. As a user, I want `:tabclose`, or `:q` on an agent tab, to close that tab and not quit hearth, so that closing a tab is safe.
8. As a user, I want to stay where I am when an agent starts, so that several agents starting at once don't pull me around.
9. As a user, I want an agent's tab to close as soon as its report reaches the parent, so that finished work doesn't pile up.
10. As a user, I want a failed or stopped agent's tab to stay for 30 seconds, so that I notice the failure before it goes.
11. As a user, I want the tab I am viewing never to close under me, and to close 30 seconds after I leave it, so that reading an agent's work isn't interrupted.
12. As a user, I want a tab I have typed into to stay until I close it, so that a conversation I started with an agent doesn't vanish.
13. As a user, I want an agent that failed for a temporary reason after doing some work to keep its tab, so that I can retry it.
14. As a user, I want agents started by agents on the same tab line, marked with the tab that started them, so that nesting doesn't hide work.
15. As a user, I want `gf` on a parent's `↳ agent` line to reopen that agent's tab, so that I can read a finished agent's transcript.

### Implementation Decisions

- **Core leaves agents out of the session list.** `Visible` skips agents whose root still exists, so the sidebar, the session picker and grep all leave them out. An agent whose parent was deleted stands alone in the sidebar. Core gives each agent's root (`Root`), the first session above it that isn't an agent, and the agents below each root (`Agents`).
- **Core keeps how each agent's last turn ended.** `report` already knows `completed`, `failed` or `stopped`. It also records the status on the agent's `Live`, so the TUI can tell a finished agent from a failed one without parsing the parent's transcript.
- **Tabs are view state and live in the TUI.** Per root session, the TUI keeps the ordered open tabs and, per tab, a close time and a pinned flag. Tab 1 is always the root. This matches the roadmap's daemon plan, where view state is per client.
- **A tab opens when the TUI first sees an agent of that root working.** Focus does not move.
- **Closing rules.** These are checked on the TUI's existing 120 ms tick. The 30 seconds is Claude Code's `jA = 30000`.
  - Report `completed`: the tab closes at once.
  - Report `failed` or `stopped`: the tab closes 30 seconds later.
  - Shown tab: it never closes. Leaving a finished agent's tab sets its close time to 30 seconds later, like Claude Code's `retain`.
  - Typed-into tab: sending a message from an agent's tab pins it until `:tabclose`, like Claude Code's `chatEngaged`.
  - The tick redraws while any tab waits to close, even when no agent runs.
  - Temporary failure after progress: the tab stays open. Core sets `Retryable` on the agent when the failure text names a rate limit, overload, a 429 or 5xx status, or a timeout, and the turn produced assistant output. A `shortcut:` comment records that each adapter should flag these once real failures are catalogued.
- **Closing a working agent's tab is refused.** The message says to stop the agent with Esc first. Closing a tab never stops an agent. On an agent's tab, `:q` closes the tab; `:qa` quits.
- **Nested agents.** They share their root's tab line. Each label names the tab that started it, for example `3 ↳2 audit deps`. There are no tabs within tabs.
- **Reopening.** `gf` on a `↳ agent` line opens that agent's tab. `gf` already existed as a key (`viewGoToFile`) but nothing handled it. The line carries the agent's session ID. Both adapters turn a reloaded `<agent-report>` message back into that line (`ReportEntry`), so `gf` works after a restart too.
- **After a restart, no agent tabs open.** Without the daemon, no agent survives a restart, so none is working. Earlier agents stay reachable through `gf`.
- **Background work is a known gap.** A Claude agent keeps Claude's background Bash and Monitor. Its tab can close while a background shell it started still runs. A `shortcut:` comment records this. The fix comes once hearth can see an agent's background work, as Claude Code's `keepaliveReasons` does.

### Testing Decisions

- **Core seam.** `Handle`-driven tests with the fake launchers cover four behaviours:
  - `Visible` leaves agents out;
  - root lookup across nested agents;
  - the agents listed under a root;
  - the report status recorded on the agent.

  Prior art: the `start_agent` tests in the core app tests.
- **TUI seam.** Tests in `internal/tui/ui_test.go` cover the following:
  - a tab opens when an agent works;
  - each closing rule, with the clock injected;
  - `gt`/`gT`/`{n}gt`;
  - refusing to close a working agent's tab;
  - `gf` reopening a tab.

  Prior art: the sidebar and key tests already there.
- **Adapter seam.** The existing transcript-loading tests of both adapters cover a reloaded report becoming the agent line.

### Settled Questions

- **Where the needs-you mark goes.** Both on the tab label and on the root's sidebar row.

### Open Questions

- **Which failures are temporary.** Core matches the failure text for now. Provider 403s are left out, because the one seen so far (Kimi K3) didn't clear on retry. Claude reports a `result` subtype plus error text; pi reports an error string. Catalogue real failures from both, then move the flag into the adapters.

### Out of Scope

- Tabs for sessions that aren't agents.
- Sidebar nesting in the style of Claude Desktop (for `start_session`; see the roadmap).
- Letting the parent's model continue a finished agent (`send_message`).
- Tracking an agent's background shells and monitors.

### Further Notes

- **Claude Code 2.1.302 subagent panel**, read from the binary. Docs: code.claude.com/docs/en/sub-agents.
  - On success, the row goes at once and the footer shows `/tasks to see subagents` for 30 seconds.
  - On failure or stop, the row gets `evictAfter = now + 30 s`.
  - The `Bdt` function skips eviction while `retain` (the user is viewing it) is set. For a completed agent, it also skips eviction while `kU` holds (`chatEngaged`, pin not forfeited) or `keepaliveReasons` is non-empty.
  - Agent-team teammates with a transient failure after progress are held (`hold_evict`).
- **Claude Desktop 2.42304.0** nests only sessions with `lineage`, `spawnedFrom.sideSession` or a fork in its sidebar (`$l`). Its Agent-tool subagents appear only as a "Running subagent" row in the transcript.
