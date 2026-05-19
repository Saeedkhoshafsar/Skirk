# Changelog

## v0.1.53 - 2026-05-19

### Throughput and quota multiplication

- **Multi-mailbox striping (opt-in)**: added `drive.extra_mailboxes` to the
  config so a tunnel can split traffic across multiple independent Google
  Drive mailboxes (each with its own OAuth refresh token / access token /
  token_command and optional folder). Lane assignment is deterministic
  (FNV-1a of the object name modulo pool size) so client and exit reach
  the same mailbox without any negotiation. Each mailbox keeps its own
  adaptive backoff: a 429 on one mailbox no longer stalls the others,
  which is exactly the property that turns extra accounts into a linear
  quota multiplier in practice. Up to 15 extra mailboxes are accepted (16
  total). The single-mailbox path is byte-for-byte unchanged: omitting
  `extra_mailboxes` leaves behaviour identical to v0.1.52.
- New `MailboxPool` type (`internal/skirk/mailbox_pool.go`) implements
  every interface the mux already type-asserts on `Data` — `BlobStore`,
  `ObjectPutStore`, `ObjectPutIDStore`, `ObjectIDReserveStore`,
  `ObjectIDStore`, `RangeObjectStore`, `FreshListStore`,
  `FreshListStatusStore`, `FreshListPageStatusStore`,
  `FreshListContainsPageStatusStore`, `ChangeFeedStore`, and the
  `WaitForDriveQuota` quota-wait contract — so wrapping is transparent
  to the rest of the codebase. Reserved fileIDs are tracked in a bounded
  FIFO index (16k entries) so subsequent PutObjectWithID / GetByID hit
  the right mailbox in O(1); on miss we fan out across mailboxes and
  treat 404/NotFound as "object lives in another mailbox", not as a
  real failure.
- Added `BlobStoreFromConfig` helper so the eight `cmd/skirk` entry
  points that currently call `StoresFromConfig` can adopt multi-mailbox
  by changing a single construction site each. The helper returns a
  `func()` cleanup that closes the primary token source and every extra
  mailbox token source so the OAuth-refresh goroutines do not outlive
  the tunnel (matches the v0.1.52 single-mailbox `Close()` discipline).

### Connection-pool throughput

- Bumped HTTP transport pool limits from 256/64 to 512/128 (max idle
  conns / per-host) and idle timeout from 90s → 120s. With multi-mailbox
  active each mailbox warms its own TLS sessions against the same Google
  host, and the old per-host cap was the easiest way to starve mux
  workers of pre-warmed connections. Costs ~1.5 KiB of kernel buffer per
  extra idle connection, which is well within the desktop/server
  profile's existing 256 MiB mux buffer budget.
- Set explicit `WriteBufferSize` / `ReadBufferSize` (64 KiB each) on the
  HTTP/1 transport and `MaxReadFrameSize = 1 MiB` on the HTTP/2 fallback
  transport. Larger socket buffers cut syscall fragmentation on bulk
  Drive media transfers, and the larger H2 frame ceiling lets a single
  Drive media download stream avoid stalling on WINDOW_UPDATE round
  trips.

### Burst-poll keeps up with download-only traffic

- `markActivity` now also refreshes `lastUploadNS`, not just
  `lastActivityNS`. Previously a download-dominated session (e.g. a
  multi-megabyte file transfer where the client sends one GET and then
  only receives data) would silently drop out of burst-poll cadence
  after `BurstPollWindow` elapsed since the last *upload* even while
  data was still actively being received. Refreshing on any frame
  activity keeps the fast poll loop engaged for as long as data is
  flowing in either direction; idle behaviour is unchanged because the
  window still expires once activity itself stops.

### Tests

- New `internal/skirk/mailbox_pool_test.go` covers `NewMailboxPool`
  validation, deterministic name → store routing, FNV distribution
  sanity (no bucket gets >60% of 4k synthetic names), out-of-range
  guard on `recordID`, round-robin spread for fileID reservation,
  bounded FIFO eviction semantics, `isProbablyMailboxMiss` substring
  classification (including the explicit negative cases for timeout,
  401, 429, and plain network errors so a real failure is never
  silently treated as "try the next mailbox"), and config validation
  for the new `extra_mailboxes` field (limit, label safety, auth
  presence). All existing tests still pass.

## v0.1.52 - 2026-05-19

### BREAKING

- **SOCKS LAN bind now requires opt-in**: the unauthenticated SOCKS server
  refuses to start on a non-loopback listen address (e.g. `0.0.0.0:1080`,
  any IPv6 ULA, or a LAN IP) unless the caller explicitly sets
  `SOCKSServer.AllowLANListen = true`. Deployments that intentionally
  exposed the SOCKS listener to other hosts will fail to start after
  upgrading until the opt-in flag is set or the listen address is moved
  back to loopback. This closes the "unauthenticated SOCKS open relay on
  the local network" footgun for default configurations.

### Memory and reliability

- Added Android-specific mux buffer caps via build tags
  (`mux_limits_android.go` vs `mux_limits_default.go`) so mobile builds use
  ~128 MiB of mux buffering instead of ~1 GiB, avoiding the platform
  low-memory killer on 2–4 GB devices while keeping desktop/server throughput
  unchanged. The active mux limits are now logged once at tunnel startup so
  the chosen profile is visible in field debug captures.
- Replaced the `markSeen` catastrophic full-map reset with a TTL-based
  three-stage eviction (10 min → 2 min → reset) so dedup state for
  legitimately in-flight Drive objects survives high-traffic bursts.
- Added a `Close()` method to `AccessTokenSource` and `DriveStore` so the
  background OAuth-refresh goroutine is cancelled cleanly on shutdown
  instead of leaking past tunnel exit, and wired `defer drive.Close()`
  into every `cmd/skirk` command that opens a DriveStore
  (`serve-client`, `serve-exit`, `client-ui`, `cleanup`, `bench-live`,
  `bench-drive`, and setup mailbox create/validate). Also fixed a latent
  leak in `StoresFromConfig` where a failed initial `Token()` call would
  drop the token source without cancelling its context.

### Routing and config

- Added an optional `Role` field to `Config` so `ApplyDefaults` can pick
  per-role defaults (e.g. `route.mode = "direct"` for exit nodes,
  `real_pinned` for clients). Existing configs without `role` behave as
  before. The same role is now propagated into the tunnel up front so
  worker-count and limiter-window calculations have the correct role from
  the very first acquire.

### Security

- Capped Drive HTTP response bodies at 32 MiB (`io.LimitReader`) to prevent
  a malicious or buggy endpoint from forcing unbounded allocations.
- Logged a warning when `token_command` contains shell metacharacters, to
  surface accidental injection vectors when the value comes from untrusted
  input, and added an opt-in `SKIRK_STRICT_TOKEN_COMMAND=1` environment
  variable that escalates the warning to a hard refusal so deployments that
  load configs from shared/untrusted sources can fail closed instead of
  exec'ing the suspicious string under `/bin/sh -lc`.
- Scrubbed the cached OAuth bearer from `AccessTokenSource.Close()` (the
  `token`, `source`, and `expiresAt` fields) so a memory dump captured
  after shutdown no longer surfaces the secret reachable from a live
  source. Go strings are immutable so the original heap object is best-
  effort, but no live `AccessTokenSource` retains the reference after
  Close.
- Added a per-session `(clientID, runID, lane, sequence)` replay map in
  `driveMux` that runs *after* `OpenEnvelope` authenticates a sealed Drive
  object. The pre-existing name-based `markSeen` map can be evaded by a
  Drive operator who renames or copies an envelope; the sequence-keyed
  map cannot, because the tuple is observed only post-AEAD verification.
  The map is TTL-compacted at 30 minutes with a 200 000-entry cap so it
  cannot grow without bound.

### Cleanup and tests

- Removed the unused `Plane` field from `muxObjectMeta` and the redundant
  `Dialer.Timeout` in `dialDirectExitTarget` (the surrounding
  `context.WithTimeout` is already authoritative).
- Simplified `tokenNeedsRefreshForRoute` so the boundary `lifetime == margin`
  case is handled by a single `<=` comparison instead of two equivalent
  branches.
- Added regression coverage for the three-stage `markSeen` TTL eviction
  (10-minute compaction first, then 2-minute compaction, then last-resort
  reset) and for AES-GCM nonce uniqueness.
- Added regression coverage for the new `(lane, seq)` replay defence
  (`TestCheckAndMarkSeqSeenDetectsReplay`, `…EvictsByTTL`) and for the
  strict `token_command` refusal and `AccessTokenSource.Close` token
  scrub (`TestTokenFromCommandStrictRefusesMetacharacters`,
  `TestAccessTokenSourceCloseScrubsToken`).

## v0.1.51 - 2026-05-19

- Changed Linux generated client profiles and sample configs to default to
  `google_front_pinned`, matching Android and Windows hostile-network behavior
  and avoiding local DNS lookups for `www.google.com` on restricted paths.
- Documented that existing Linux profiles generated by older versions can keep
  using `google_front`; regenerate the kit or pass
  `--route-mode google_front_pinned --google-ip 216.239.38.120` to use the new
  pinned route.

## v0.1.50 - 2026-05-18

- Restart the interactive Linux menu from the newly installed binary after a
  menu-driven Skirk update, so freshly added menu actions are visible
  immediately instead of requiring users to quit and rerun `skirk`.

## v0.1.49 - 2026-05-18

- Added an outbound-proxy menu action to uninstall Skirk-managed WARP
  wireproxy, remove its systemd dependency from the exit service, and switch
  the exit back to direct mode.
- Hardened WARP wireproxy removal so leftover config and helper binaries are
  cleaned even if the wireproxy systemd unit was already missing.
- Hardened WARP setup and updates with loopback-only managed SOCKS binds,
  preflight checks before root writes, an unprivileged `skirk-wireproxy`
  service user, Skirk-owned artifact manifests, and stricter menu-update
  environment sanitization.

## v0.1.48 - 2026-05-18

- Added pinned installer version arguments.
- Kept Android release downloads on the stable `skirk-android-arm64.apk` asset
  name because the GitHub release tag already carries the version.
- Added menu actions for updating Skirk and configuring exit outbound proxy,
  including optional WARP wireproxy setup.
- Simplified Android status hierarchy so the app shows one primary connection
  state and keeps the version visible without repeating status labels.
- Tightened Windows desktop status wording and version visibility.

## v0.1.47 - 2026-05-17

- Added Drive-wide quota backoff so 403/429 rate-limit responses stop poll,
  upload, download, and cleanup retry storms instead of multiplying them.
- Reworked mux v4 priority so small streams stay ordered in the priority lane
  while bulk streams demote cleanly to normal traffic, preventing priority and
  normal frames from overtaking each other.
- Raised auto-profile polling defaults to reduce Drive list pressure across
  Android, Windows, and generated setup profiles.
- Added targeted mux gap repair and regression tests for missing Drive objects,
  ordered small-stream priority, bulk fairness, and Drive rate-limit handling.
- Suppressed expected Android non-DNS UDP refusal noise while keeping the VPN
  IPv4/TCP fallback policy for QUIC-heavy apps.
- Clarified setup docs that easy OAuth shares Skirk project quota and personal
  OAuth only isolates quota when users create their own Google Cloud project.

## v0.1.46 - 2026-05-17

- Reduced Drive mailbox quota pressure in VPN mode by disabling burst polling
  for Android and Windows VPN sidecars, lowering VPN worker concurrency, and
  pacing normal active polling while keeping proxy mode aggressive.
- Hardened mux v4 under long-running and multi-client traffic with bounded
  bootstrap priority data, idempotent reserved-ID normal uploads when available,
  stale Drive object handling, and receive-gap timeout/repair behavior.
- Made Drive cleanup less disruptive by tightening janitor defaults and
  avoiding foreground stalls from cleanup pressure.
- Fixed Android VPN connect/disconnect lifecycle races so repeated connect
  taps are idempotent and disconnect closes the Android TUN descriptor before
  stopping tun2socks and the Skirk sidecar.
- Added regression coverage for stale Drive objects, reserved upload IDs, and
  Drive ID-generation fallback.

## v0.1.45 - 2026-05-17

- Switched personal Google OAuth setup to the Desktop app authorization-code
  flow with PKCE and VPS paste-back support for redirected localhost URLs.
- Kept easy Skirk OAuth on Google's device-code flow and restored the built-in
  release requirement for both OAuth client ID and client secret.
- Updated setup docs and wizard text to stop recommending TV/Limited Input
  clients for personal OAuth unless a client secret is available and
  `--oauth-flow device` is explicitly selected.

## v0.1.44 - 2026-05-17

- Allowed personal Google OAuth clients to use a client ID without a client
  secret, matching Google's public-client behavior.
- Updated setup docs, wizard prompts, and release build checks so client
  secrets are used when available but are not required.

## v0.1.43 - 2026-05-17

- Removed the release publish job's `actions/download-artifact` dependency and
  switched to GitHub CLI artifact download to avoid a Node deprecation warning
  emitted by that action.
- Suppressed Git checkout initialization hints by configuring the default branch
  before checkout in CI and release jobs.

## v0.1.42 - 2026-05-17

- Pinned GitHub Actions CI and release runners to explicit stable images
  (`ubuntu-24.04` and `windows-2022`) to avoid floating-runner migration
  notices in release builds.

## v0.1.41 - 2026-05-17

- Refreshed the release workflow onto GitHub's Node 24 artifact actions so
  the latest release is produced without Node 20 deprecation warnings.
- Kept the Android release path on `assembleRelease` with required keystore
  secrets, signature verification, checksums, and artifact attestations.

## v0.1.40 - 2026-05-17

- Added `skirk service` and expanded the operator menu for setup, systemd
  service lifecycle, Drive cleanup, OAuth revocation, and local kit deletion.
- Stopped new setup runs from launching the blocked default Google Cloud SDK
  OAuth client for Drive scopes; release builds can now use Skirk's built-in
  device OAuth client through Google's URL/code device flow.
- Switched the public device-code setup scope to `drive.file` and a
  Skirk-created Drive mailbox folder, because Google rejects `drive.appdata`
  during the tested device-code request.
- Clarified Windows release packaging so the portable desktop zip is the GUI
  app and `skirk-windows-amd64.zip` is documented as CLI-only.
- Clarified install commands to use the absolute installed binary path when
  shell `PATH` propagation is unreliable.
- Added donation placeholders to the README.
- Fixed Android sidecar startup validation so stale local listeners are not
  accepted as a healthy new engine, without using an Android parent-death signal
  that can terminate valid app-launched sidecars.
- Added Drive Mux v4 client/run namespacing so the same copied `skirk:` profile
  can run on multiple devices at the same time without response races.
- Updated Android and Windows clients to pass stable per-profile client IDs to
  the Skirk sidecar.
- Added Drive Mux v4 documentation as the single production transport.
- Added docs for exit-side proxy forwarding, mailbox janitor cleanup, live
  benchmarks, quota telemetry, and Drive Changes based discovery.
- Hardened Android VPN mode around the proven Drive transport flags, IPv4-only
  routing, TCP fallback for app media traffic, and real-device Reels plus bulk
  download validation.
- Added SOCKS DNS/UDP tests covering AAAA suppression and non-DNS UDP refusal.
- Switched Android release assets from debug APKs to release-signed APKs and
  added GitHub artifact attestations for published release archives/APK.
- Updated setup docs around one-line `skirk:` profiles, `serve-client`,
  `serve-exit`, custom OAuth setup, and Drive mailbox folders.
- Removed stale references to alternate runtime control lanes and visible Drive
  folder cleanup from user-facing docs.

## v0.1.3 - 2026-05-02

- Replaced noisy Google Cloud CLI setup with quiet archive installation.
- Fixed Ctrl-C handling for Skirk menu prompts and long-running commands.

## v0.1.2 - 2026-05-02

- Added automatic Google Cloud CLI install/check during server setup.
- Added one-line `.skirk` client configs for paste-friendly sharing.
- Added config export/decode commands while keeping JSON compatibility.

## v0.1.1 - 2026-05-02

- Added official Skirk logo assets.
- Added the Skirk terminal banner.
- Updated desktop and Android launcher icons.

## v0.1.0 - 2026-05-02

- Added Go Skirk CLI with a Google Drive mailbox transport.
- Added one-command Google kit setup and config generation.
- Added Linux SOCKS5 client mode and exit mode.
- Added optional browser dashboard and Windows desktop wrapper.
- Added Android VpnService scaffold.
- Added Linux installer, release packaging, CI, and preflight checks.
