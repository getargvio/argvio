# Install journeys: activation, friction and lifecycle analytics

Status: proposal, nothing implemented. Written after reading
`schema/allowlist/v1.yaml`, both SDKs (`argvio-go`, `argvio-node`),
migrations 0002–0005 and `internal/storage/query.go`.

## Goal

Move the dashboard from "how much is each command used" to "do new users
succeed, where do they give up, and what is breaking for whom". That covers
the Funnel & Lifecycle, UX & Friction, and parts of the Usage and Errors
categories of metric gaps the UI showed (now tracked as issues in
`getargvio/ui`).

## What the code already does (and the TODO gets wrong)

Both SDKs emit the same four log events plus a command span and metrics:

| Signal | Carries | Tier |
| --- | --- | --- |
| `cli.session.started` | `session_id`, `install_id` | basic+ for ids |
| `cli.command.invoked` | command, flag names, `session_id` | basic+ for the latter two |
| `cli.command.<name>` span | command, exit code, duration | all |
| `cli.help.viewed` | command, help flag | all |
| `cli.error.raised` | command, `error.type` (coarse category) | full+ |

So several items listed as "needs new telemetry" are **query gaps only**:

- **Help-flag usage rate**: `cli.help.viewed` is already ingested. Needs an
  endpoint (help views / invocations per command).
- **Network vs local error, timeout frequency, canceled**: both SDKs
  classify errors (vendor-assigned) as `user | validation | network | auth | internal | timeout |
  canceled | unknown` and send it as `cli.error.type`. Needs an endpoint over
  `cli.error.raised` logs. Limitation: only emitted at `full`+ tier.
- **Abandonment (Ctrl+C)**: spans already carry the exit code at every
  tier, plus the `canceled` category at `full`+. Counting exit 130 (SIGINT)
  works only if the vendor's CLI actually exits 130 on interrupt, which the
  SDKs don't enforce, so treat it as a lower bound. No SDK change needed for
  a first version.

## What is actually missing

1. **No join between a command's outcome and who ran it.** Spans carry the
   exit code but no `session_id`/`install_id`; `cli.command.invoked` carries
   `session_id` but not `install_id` (the allowlist permits it, the SDKs
   don't send it). So "this install's first successful command" cannot be
   computed. `cli.command.completed` is allowlisted (command, exit code,
   duration) but neither SDK emits it, and it has no id attributes.
2. **No durable per-install state.** Raw logs are kept 90 days and spans 30
   (`0004_compression_retention.up.sql`). `Retention` already notes that
   first-seen is "bounded by raw log retention (installs older than that look
   new)". Any first-run, activation or upgrade metric computed from raw logs
   inherits that error.
3. **A process is a session.** CLI invocations are short, so
   `session_id` is usually one command. Funnels and "trial-and-error" must
   be keyed by `install_id` across invocations, not by session. This
   corrects the earlier idea of an SDK "session summary at exit": it would
   almost always contain a single command.

## Proposal

### 1. Install state table (server-derived, small)

`installs(tenant_id, install_id, first_seen, first_success_at, last_seen,
first_version, last_version, last_os, last_arch, install_method NULL)`,
upserted at ingest from `cli.session.started` and `cli.command.completed`.

Derives without any further SDK change:
- first-run vs returning (a session whose time equals `first_seen`'s
  bucket);
- install → first command → first success funnel and time-to-activation
  (`first_success_at - first_seen`);
- upgrade/downgrade (`last_version` changes; semver compare server-side).

Hash-partitioned by tenant like the other tables; one row per install, so
it stays small relative to the hypertables.

### 2. SDK changes (Go and Node, same release train)

- Emit `cli.command.completed` from `RecordCommand` with exit code,
  duration, `session_id` and (basic+) `install_id`.
- Send `install_id` on `cli.command.invoked` (already allowlisted).
- Allowlist: add `cli.session_id` / `cli.install_id` to
  `cli.command.completed`, bump nothing else. This is a data-only change
  to `v1.yaml` per `docs/allowlist.md`.
- `install_method`: the CLI itself can't know; add an SDK option
  (`WithInstallMethod`, default unset) that the vendor fills from build
  metadata, and send it on `cli.session.started`. The `cli.install.completed`
  event is only emittable by an installer script, so leave it for vendors who
  have one.
- Typos: do not send the mistyped text. Emit only `cli.error.type=user`
  plus a bounded `cli.command.unknown=true`-style flag on the invocation,
  and, when the framework can suggest one, the allowlisted nearest command
  name. Needs a new allowlisted attribute.

### 3. Endpoints (all tenant-scoped, same filters as today)

- `GET /v1/metrics/funnel`: counts of installs that reached each stage in
  the window.
- `GET /v1/metrics/activation`: time-to-first-success distribution.
- `GET /v1/metrics/friction`: per command, rates of help views, abandonment
  (exit 130), repeated failure of the same command by one install within a
  short window.
- `GET /v1/metrics/error-types`: distribution of `cli.error.type`,
  time-bucketed.
- `GET /v1/metrics/version-transitions`: upgrades/downgrades by version
  pair.

### 4. UI

Fill in `FunnelView`, `UxFrictionView` and the Usage gaps, removing the
matching `GapPlaceholder`s. Every journey chart shows its coverage: the
share of sessions that carried an `install_id`, since anonymous-tier
sessions can't be followed.

### 5. Prerequisite for slicing

`attr.*`, `attr_contains.*` and `attr_in.*` apply to `/v1/traces` only; the
aggregate endpoints ignore them. Install method (and any later attribute)
can only slice charts if it is promoted to a rollup dimension. Plan: add
`install_method` as a column on `installs` and join it into the new
endpoints; decide separately whether to generalize.

## Decisions (agreed)

1. **Retention vs `installs`**: purge rows by `last_seen` against the
   tenant's retention, and treat "returning after the retention window" as
   new, with that stated in the UI.
2. **Tier of error category**: lower `cli.error.type` from `full` to
   `basic` in the allowlist and in both SDKs (which currently gate the
   `cli.error.raised` event at `full`). The raw message and stack stay at
   `full`/`optin_plus`.
3. **Coverage**: journey metrics need `basic`+ (`install_id`), so they cover
   consenting users only. The UI shows the share of sessions that carried an
   `install_id`.

## Order of work

1. Backend only (ships value with no SDK release): `error-types`,
   help rate and abandonment in `friction`, from existing events and exit
   codes.
2. Allowlist change plus SDK changes in both repos
   (`cli.command.completed`, ids on invoked, `WithInstallMethod`).
3. `installs` table, migration and ingest upsert; backfill from the last 90
   days of `cli.session.started`.
4. `funnel`, `activation`, `version-transitions` endpoints.
5. UI views.

Steps 1 and 2 are independent and can be built in parallel.

## Progress

- Step 1 (partial): `/v1/metrics/error-types` and `/v1/metrics/friction`
  (help views, interrupted runs) are implemented with the Errors and UX
  Friction views. Repeated failure of the same command by one install is
  not in `friction` yet; it needs `install_id` on command outcomes (step 2).
- Step 2 (done, unreleased): allowlist and both SDKs.
  - `cli.error.type` is now `basic` tier, and both SDKs emit
    `cli.error.raised` at `basic`+ (previously `full`+).
  - `cli.command.completed` is emitted by `RecordCommand`/`recordCommand`
    with exit code, duration, and session and install IDs at `basic`+.
  - `cli.command.invoked` and `cli.error.raised` now carry the install ID.
  - `WithInstallMethod` / `installMethod` sends `cli.install.method` on
    `cli.session.started` at every tier.
  - Not done from this step: the typo signal (needs a new allowlisted
    attribute and framework support to detect an unknown command).
  - The SDK change is user-visible for `basic` users: the error category
    now leaves the machine at `basic`. Release notes should say so.
- Step 3 (done, unreleased): `installs` and `install_version_changes`
  tables (`migrations/0006`), ingest upsert, `argvio installs backfill`, and
  the install sweep in `argvio retention sweep` (see `docs/schema.md`,
  "Install state").
  - Deploy order: migrate, deploy the new server, then run
    `argvio installs backfill` once. Installs seen only before the deploy are
    otherwise missing until they next appear.
  - `install_method` is stored from `cli.session.started`, latest wins.
- Step 4 (done, unreleased): read endpoints over install state, all
  tenant-scoped with day/week/month buckets (default week):
  - `GET /v1/metrics/install-funnel`: per first-seen bucket, installs, ran a
    command, first success.
  - `GET /v1/metrics/install-activation`: cohort size, activated count, and
    p50/p90 seconds to first success (activated installs only).
  - `GET /v1/metrics/version-changes`: upgrades and downgrades by version pair.
  - `GET /v1/metrics/install-coverage`: sessions vs sessions carrying an
    install id (raw logs, bounded by log retention), for the "share of
    sessions followed" label.
  - Filter semantics: `cli_version` is first-seen version for funnel and
    activation, the version moved to for version-changes; `os`/`arch` are the
    install's latest values; `command` and `is_ci` don't apply (installs
    carry neither). Recent cohorts are still maturing, so later stages are
    counted whenever they happened.
  - Not done: the "repeated failure of the same command by one install"
    friction signal, which needs a per-install command history that the
    installs table doesn't keep. Doing it from raw `cli.command.completed`
    logs (bounded by log retention) is the likely route.
- Step 5 (done, unreleased, in the `ui` repo): Funnel & Lifecycle shows the
  install funnel, time to first success and version changes; UX & Friction
  shows activation time. Every install-based panel (including the existing
  active installs and retention) states the share of sessions that carried an
  install id. Not visually checked against live data.
- Follow-up (done, unreleased): two more endpoints over install state, both
  with Usage panels:
  - `GET /v1/metrics/install-methods`: installs per first-seen bucket by
    vendor-reported install method (null when never reported).
  - `GET /v1/metrics/new-vs-returning`: distinct installs active per bucket,
    split by first seen in that bucket vs earlier (raw session logs joined to
    `installs`, so bounded by log retention; installs without a row are left
    out).
  - The dashboard's "gap" banners now say why: needs new telemetry, no view
    yet (crash payloads exist for opt-in sessions), or product decision.
- Validated end to end against a local stack with both demo CLIs. The
  new-vs-returning demo data backdates five installs' first sighting, since
  the CLIs can't produce an earlier first run.
- Remaining from this design: typo detection (new allowlisted attribute plus
  framework support), repeated-failure friction signal (per-install command
  history), session length / command sequences, feature discovery.
