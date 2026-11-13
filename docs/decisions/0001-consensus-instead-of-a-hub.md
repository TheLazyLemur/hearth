# ADR-0001: Multi-machine by consensus, not a hub

- **Status:** Accepted
- **Date:** 2026-11-14
- **Supersedes:** the hub design in `docs/ROADMAP.md` §5 (hub mode, routing through the hub, the hub as credential broker and memory source of truth).

## Context

- The roadmap's multi-machine plan made one daemon the hub: it merged every worker's snapshot, routed messages between machines, held the long-lived secrets and the source of truth for memory. Running sessions carried on without it, but nothing else did: no new sessions on other machines, no routing, no memory sync, no failover.
- baton (`~/Programming/tools/baton`) solves the same problem without a hub, and is built and tested live across a Mac, a Linux desktop and two cloud voters:
  - one owner per session per term, from etcd;
  - a fence everywhere else;
  - checkpoints replicated before etcd points at them;
  - crash takeover in 20–40 s;
  - planned moves;
  - memory and config sync;
  - prompt-cache hits after every same-account move.
- baton's own next plans were a hexagonal rewrite, an Ultraviolet TUI, a mobile client and other agents: what hearth already has. Carrying on with both would build each half twice.

## Decision

- **No hub.** Every daemon is equal. The cluster is baton's: embedded etcd on three voters, mutual TLS between daemons, ownership by term with fencing and self-fencing, and checkpoint replication before publication.
- **baton becomes hearth's cluster layer,** behind a port in `core`. Its rules come with it (one owner per term; a committed checkpoint before any state-changing tool; etcd only points at replicated objects; history is never edited; path identity, or whatever the path spike replaces it with), and so do their tests.
- **The barrier moves to hearth's tool gate,** so it covers every backend.
- **Clients keep hearth's token pairing;** only daemons use the cluster's certificates.
- **baton is frozen** apart from fixes, and stays the running reference until hearth passes baton's live tests (crash takeover, planned move, remote viewing).

## Alternatives considered

- **Hub (the roadmap's plan):** simpler to build, but the hub is a single point for everything except sessions already running, and failover would need a second mechanism anyway.
- **Keep the projects separate,** with hearth as a client of baton: two daemons per machine, two session models, and baton still lacking a UI of its own.

## Consequences

- **Easier:** no single machine to keep alive; sessions survive any one machine; one product instead of two.
- **Harder:**
  - etcd voters to run (three, in two places at least);
  - certificates and join tokens to manage;
  - hearth's store to change from "the daemon writes" to "the owner writes, checkpoints replicate, takeover restores".
- **Open:**
  - the path spike decides where multi-machine sessions live;
  - memory conflict handling and where the credential broker lives are open questions in the roadmap.
