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

Backends to add once the shape holds: Codex, Gemini, Cursor, OpenCode, and any ACP agent through a
generic ACP backend (open as #9).

## Later

Features hearth wants that aren't built or planned yet, kept here so none are lost.
Each needs its own spec before it is built; order within a section is rough priority.

### Session list

Every session is opened to be closed: the list shows only what still needs the user.

- Archive with undo. Archived sessions sit at the bottom of the list.
- Snooze a session until a time; it comes back on its own.
- Auto-archive when the session's PR merges, and after N idle days (default 3); both configurable,
  and off per session.
- A session that needs the user (a permission, a question) jumps to the top with a marker.
- Optional running group that hides sessions while they run; starred sessions stay visible.
- Send in the background: start a session or message without opening it.
- Select several sessions and archive, snooze or regenerate titles at once.
- Drafts that survive switching away and can be moved, images included, to another session.
- Interrupt or queue for mid-turn messages, as a setting with a key to swap.

### Orchestration

- Expose hearth over MCP so other agents and apps can list, start, message and archive sessions,
  as an external client of the daemon. Scoped to what each connection is allowed.
- Any session can find and message any other, across projects, and start sessions in other
  projects. Widens §2's family-only tools.
- Drag a session into another to give it that session's context.
- Move a running session to another backend or model, carrying its context over.
- Several instances of one backend, each with its own home directory, env and account, for
  rotating subscriptions.

### Scheduled and triggered sessions

- Run a session on a schedule; it reappears in the session list when it has something to say.
- Hold a message until a rate limit resets, then send it.
- Start a session from a webhook (PR opened, Stripe event, a curl), with an optional hold and
  replay window when the daemon is down.

### Git and PRs

- Link a session to the PRs it opened.
- PR list and viewer in every client: diff, checks, merge; sort by merge readiness.
- PR babysitting built in, so agents don't each reinvent polling, and cheap on API rate limits.
- Providers past GitHub: GitLab, Bitbucket, Forgejo, Azure DevOps.

### Projects

- Sessions with no project.
- New project from the client: name it, pick its host, optionally create the remote repo.
- Worktree policy per project: always, never, ask.
- Project icon from its favicon, or picked.

### Hosts

hearth stays transport-agnostic: Tailscale, SSH or a tunnel is the user's to bring. On top of that:

- Several hosts, one client: each host runs a daemon; pick the host per session, with each host's
  load shown and an option to route to the least loaded.
- More than one route to a host (tailnet, LAN, SSH), tried in order, with permissions per route.
- Move a project to another host: clone it there, optionally copying gitignored files like
  `.env`.
- Update hearth and the agent CLIs on every host at once.
- Open a session's folder in an editor, over SSH when it lives elsewhere.
- Dedicate hosts to roles, e.g. a Mac other hosts use to run iOS builds.

### Rendering and input

- Agents can render HTML: charts, diagrams, interactive widgets, shown inline in the web client.
- Attachments saved to a path the agent can use, not only shown to the model, so it can put an
  image in a PR. Images and video viewable over remote connections.
- Annotate: select text in a transcript, comment, and send the comments with the next message.
- Snapshot: a global hotkey captures the focused app's content into a new session.
- Simulators and a headless browser running on another host, viewed and driven from the client.

### Usage

- Tokens and limits per account and backend, read from each backend's own records so usage outside
  hearth counts too.

### Settings and look

- Settings scoped to everywhere, a host, or a project, with a view of what is set where.
- Configurable keybindings.
- Themes, including VS Code theme import, and custom fonts.

### Support

- `hearth triage`: hands logs and setup to an agent to fix a broken install, and drafts an issue
  when the bug is hearth's.

## Deferred until it hurts

- Transcript grep loads every transcript on the UI goroutine; prefilter with ripgrep and parse only
  the hits, off the UI goroutine.
- Fuzzy matching is greedy; the markdown cache clears wholesale at 500 entries.
