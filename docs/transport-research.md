# Transport Research

This document records the current transport conclusions so future changes do
not repeat failed protocol shapes. Raw experiment artifacts live in the ignored
`.skirk-runs/` directory and are intentionally not committed.

## Goal

Skirk needs a generic SOCKS/HTTP/VPN transport over Google Drive for hostile
paths where the Google API route must use the configured Google-looking fronted
or pinned path. The target is:

- lowest practical latency;
- highest practical throughput;
- stable browsing during bulk downloads;
- multiple clients on the same generated profile;
- no website, hostname, path, content-type, or app-specific filters.

## Current Answer

Mux v4 is the best proven default under the current constraints:

- one Google Drive mailbox;
- current credential scope and setup model;
- Drive `appDataFolder` runtime objects;
- prefix-scoped `files.list` discovery;
- no inbound connectivity requirement for clients;
- Google-fronted/pinned client API route when needed.

That does not mean muxv4 is theoretically optimal forever. It means the tested
alternatives have not beaten it on the workload that matters: normal browsing
and media behavior while bulk downloads are active.

## Why Large muxv4-Only Gains Are Unlikely

Mux v4 already spends most of its engineering budget on the two things that
matter most for Drive:

1. reduce object count by coalescing many streams into a few lane objects;
2. keep discovery simple by listing one direction prefix and downloading by
   file ID.

Pushing muxv4 harder can still move the ceiling, but the expected gains are
incremental because the carrier still requires whole-object upload, Drive
visibility, prefix list, media download, and cleanup. Larger objects can improve
bulk throughput, but they increase head-of-line delay and reassembly pressure.
Smaller objects improve interactivity, but they add Drive calls and can collapse
throughput.

A "way above" result is more likely to require changing a core constraint:

- multiple independent Drive mailboxes or credentials;
- a broader OAuth scope with separate visible data objects and appData control;
- a non-Drive carrier for data;
- a public webhook or inbound control plane;
- a different storage provider with lower object visibility latency.

Those are valid future product choices, but they are not muxv4-only tuning.

### v0.1.53/v0.1.54 update: multi-mailbox is now available (opt-in)

The first item on the "way above" list — multiple independent Drive
mailboxes — landed in v0.1.53 as `drive.extra_mailboxes` and the
`MailboxPool` wrapper. v0.1.54 wires the pool into the three runtime
commands (`serve-client`, `serve-exit`, `bench-live`) via
`BlobStoreWithPrimaryFromConfig` so the feature is reachable from the
CLI without further code changes; admin paths (`revoke`, `cleanup`,
`bench-drive`, the setup wizard) stay primary-only by design. The pool stripes traffic across mailboxes using
deterministic FNV-1a routing on the object name, which lets both ends
agree on which mailbox carries which lane without any negotiation, and
keeps every mailbox's adaptive backoff independent so a 429 on one
account does not stall the others. Single-mailbox configs are
unaffected: the pool is only constructed when at least one extra
mailbox is configured, and the per-mailbox HTTP transport tuning
(MaxIdleConnsPerHost = 128, 64 KiB socket buffers, 1 MiB HTTP/2
read-frame ceiling) is shared with the single-mailbox path so any
existing deployment also benefits from the reduced syscall and
handshake pressure.

The pool is intentionally conservative on the rest of the surface:
change-feed support is suppressed when more than one mailbox is active
(the mux already falls back to merged FreshList* fan-outs in that
case), pagination is collapsed into a single merged page, and fileID →
mailbox mappings are kept in a bounded FIFO index so the resolution
hot path stays O(1) without growing unbounded for long-running
tunnels. Operators should treat the per-mailbox count as the
quota-multiplier ceiling: N mailboxes give roughly N× the per-minute
Drive API call budget under the same workload, but mux object size,
visibility delay, and cleanup overhead per object are unchanged.

## Rejected Protocol Shapes

Several protocol families were tested or primitive-tested and rejected because
they failed mixed workload gates, added Drive call pressure, or increased tail
latency:

- control/data split designs that added extra control uploads before bytes were
  usable;
- change-feed designs that lost prefix locality and had to filter unrelated
  appData changes;
- range-read slab designs that proved the safety primitive but paid too much
  control-plane overhead;
- strict-priority, credit, and mailbag designs that added small reverse-control
  objects or worse tail behavior;
- generated-ID rendezvous and optimistic metadata polling designs that created
  404 and metadata-call pressure;
- resumable upload and mutable update-slot designs that made Drive write tails
  worse for the hot path.

The repeated failure pattern is consistent: candidates that look cleaner inside
the mux often create more Drive objects, more list/change pages, more metadata
polls, or worse upload tails. Those costs dominate the live transport. Exact raw
artifacts stay in `.skirk-runs/` rather than public docs.

## Drive Primitive Lessons

`files.list` on a narrow prefix remains the best proven hot discovery path.
`changes.list` is durable, but it is scoped by Drive space rather than by Skirk
object prefix, so appData data objects pollute the change feed.

Generated IDs are useful for idempotent upload retry and manifest references.
They do not make new objects visible faster by themselves.

Range reads are safe only when Skirk validates HTTP `206 Content-Range` and
authenticates independently encrypted fragments. The primitive works, but tested
range transports added enough control overhead that they lost to muxv4.

Resumable upload is useful for large unreliable file uploads in general Drive
applications. In Skirk's hot path it adds an initiation round trip and did not
beat multipart upload for the tested chunk sizes.

`files.update` is a whole-file media update. It avoids create/list/delete churn
but creates expensive revision/write tails and is not a good byte-queue
primitive.

## Promotion Gates

Any future transport must beat same-day muxv4 controls before becoming the
default:

- 100 MiB mixed bulk plus small probes;
- 1 GiB mixed bulk plus small probes;
- browser/media overlap while bulk is active;
- five parallel 100 MiB clients;
- zero terminal stream failures;
- no sustained increase in Drive API errors versus the same-day muxv4 control;
- no gap-repair loop or skipped-object behavior;
- tiny downstream objects below 5% of normal objects;
- Drive calls and estimated quota units per GiB no more than 1.15x muxv4;
- cleanup leaves no stale active-prefix debt.

Single-stream throughput is not enough. A candidate that wins a raw download
test but freezes browsing should be rejected.

## Mux v4 Work Worth Doing

Muxv4 still deserves careful incremental work:

- better structured observability for queue depth, object size, gap age, socket
  write blocking, and cleanup backlog;
- same-day paired benchmark harnesses that run real concurrent small and bulk
  traffic;
- conservative object-size and receive-window experiments with hard rollback
  gates;
- restart and cleanup soak tests;
- documentation that keeps user-facing performance expectations honest.

Expected gains from those changes are stability and smaller tail latency first,
then moderate throughput improvements. They should not be described as a
guaranteed path to an order-of-magnitude speedup under the current Drive-only
constraint.
