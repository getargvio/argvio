# Install journeys: activation, friction and lifecycle analytics

Status: proposal, nothing implemented. Written after reading
`schema/allowlist/v1.yaml`, both SDKs (`argvio-go`, `argvio-node`),
migrations 0002–0005 and `internal/storage/query.go`.

## Goal

Move the analytics from "how much is each command used" to "do new users
succeed, where do they give up, and what is breaking for whom". That covers
funnel and lifecycle, UX friction, and parts of the usage and error
analytics.

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
- `GET /v1/metrics/friction`: per command, rates of help views and
  abandonment (exit 130). Retries and repeated failure of the same command by
  one install are in `retries` (section 7).
- `GET /v1/metrics/error-types`: distribution of `cli.error.type`,
  time-bucketed; `group_by=reason` splits it by the vendor's finer
  `cli.error.reason` (docs/allowlist.md "Error reasons").
- `GET /v1/metrics/error-fingerprints`: top failure causes, grouped by a
  fingerprint of the normalized stack or message, ranked by affected
  installs (full tier and above; docs/schema.md "Error fingerprints").
- `GET /v1/metrics/version-transitions`: upgrades/downgrades by version
  pair.

### 4. Coverage

Every install-based endpoint is only as complete as the share of sessions
that carried an `install_id`, since anonymous-tier sessions can't be
followed. `/v1/metrics/install-coverage` exposes that share so consumers
can state it next to the numbers.

### 5. Prerequisite for slicing

`attr.*`, `attr_contains.*` and `attr_in.*` apply to `/v1/traces` only; the
aggregate endpoints reject them with a 400 rather than ignore them. Install method (and any later attribute)
can only slice charts if it is promoted to a rollup dimension. Plan: add
`install_method` as a column on `installs` and join it into the new
endpoints; decide separately whether to generalize.

### 6. Sessions

A process is rarely a useful session (see "What the code already does"), so a
**session** here is a burst of `cli.command.completed` events from one
`cli.install_id`. A gap of **more than 30 minutes** (`storage.SessionGap`)
between two consecutive events of that install starts a new session; a gap of
exactly 30 minutes does not. The gap is a constant, not a query parameter, and
every response states it as `gap_seconds`.

- **Length** is the last event time minus the first, so a single-command
  session is 0. A burst whose commands each follow the last within 30 minutes
  can run longer than 30 minutes.
- **Data source**: raw `cli.command.completed` logs, sessionized at query time.
  The server keeps no per-install command history (the `installs` table has
  first/last seen and first success only), so results are bounded by log
  retention (~90 days), like `new-vs-returning` and `install-coverage`.
  A persisted per-install history is out of scope.
- **Who is seen**: only events with a `cli.install_id` (`basic` consent tier and
  above). Anonymous-tier usage can't be tied to an install and is invisible
  here; `/v1/metrics/install-coverage` says how much traffic that is.
- **Window and filters**: sessions are clipped to the query window, so one
  running across `from` or `to` is measured by the events inside it. The
  `command`, `cli_version`, `os`, `arch` and `is_ci` filters pick the *events*
  sessions are built from, before the gap rule is applied: `command=deploy`
  measures bursts of `deploy` runs, and a session interrupted by other commands
  may split. `exit_code` doesn't apply and `attr.*` is rejected with a 400.
- Events with the same timestamp are ordered by command name, which only
  matters for which of them comes first in a sequence.

Endpoints, both tenant-scoped and not time-bucketed:

- `GET /v1/metrics/session-length`: session count, median length, and sessions
  per length bucket (`0`, `<1m`, `1-5m`, `5-15m`, `15-30m`, `30m+`; lower bound
  inclusive, upper exclusive; every bucket is always listed).
- `GET /v1/metrics/session-sequences`: the top-K most frequent ordered command
  sequences with their session counts (`limit`/`offset` page the ranking, most
  sessions first, ties by command names). A session's sequence is:
  1. its commands in time order, events without a command name left out;
  2. **consecutive repeats collapsed**: `deploy, deploy, status` is `deploy,
     status`. Retries of one command are a friction signal (#16) rather than a
     journey step, and would otherwise split one journey into many sequences;
  3. **cut to the first 5 commands** (`storage.MaxSequenceLength`), so long
     sessions are grouped by how they began instead of each being unique. An
     n-gram approach would count a session many times and not add up to the
     session count. The cut means a 5-command sequence may be a session that
     went on; the response doesn't flag it.

  Single-command sessions outnumber everything else, so `min_length` (1-5)
  sets shorter sequences aside; it applies after the collapsing and the cut.

### 7. Retries and repeated failures

A user retrying a failing command looks, per run, like an independent
invocation. `GET /v1/metrics/retries` tells them apart from the same raw
`cli.command.completed` logs as sessions: one `cli.install_id`, one command
name, runs in time order. A run **failed** when its exit code is non-zero.

- **Window W**: `storage.RetryWindow`, 10 minutes, or `window_seconds` (1 to
  3600). Every comparison against W is inclusive: a rerun exactly W after a
  failure is a retry, one a second later is not. The response states W as
  `window_seconds`.
- **Retried failure**: the install's next run of the same command is at most W
  after the failure, whatever its outcome. Other commands in between don't
  matter, and one command's runs never affect another's.
- **Retry sequence**: a failure and the runs after it, each within W of a
  failed run before it. A success ends the sequence, as does a gap over W or
  the end of the data. A sequence of two or more runs is counted in
  `retry_sequences`; it **recovered** if it ended in a success. A failure
  with no rerun within W is a one-run sequence: failed, not retried.
- **Failure streak**: the failures in one sequence. `longest_failure_streak`
  is the largest per command. Streaks chain, so failures 8 minutes apart make
  a streak of 4 over 24 minutes.
- **Repeated failure**: an install with `storage.RepeatedFailureCount` (3)
  consecutive failures of the command whose first and last lie within W,
  counted in `repeated_failure_installs`. This is stricter than a streak of
  3: the example above is a streak of 4 but not a repeated failure.
- **Out of scope**: "variations" (the same command with different flags).
  `cli.command.completed` has no flag names; `cli.command.invoked` has them
  but no exit code, so it would take a join on `cli.session_id`.
- **Who is seen, retention, filters**: as for sessions. Anonymous-tier
  failures have no install id and are invisible (see `install-coverage`); a
  completion without an exit code is skipped. The `command`, `cli_version`,
  `os`, `arch` and `is_ci` filters pick the runs sequences are built from, so
  a filter that drops some runs can hide a retry. `exit_code` doesn't apply and
  `attr.*` is rejected with a 400.
- **Response**: not time-bucketed; `data` holds `window_seconds`,
  `repeated_failure_count` and `commands`, one row per command with at least
  one failure, most failures first (`limit`/`offset` page them).
  `retry_rate` is `retried_count / failed_count`.

### 8. Cold vs. warm latency

The first run of a CLI is often much slower than later ones: caches are empty,
config is created, and an upgrade invalidates what the last version built.
`/v1/metrics/latency` mixes them. `GET /v1/metrics/latency-cold-warm` splits
`cli.command.duration_ms` by whether the run was cold, from raw
`cli.command.completed` logs joined to install state.

- **Why server-derived** (rather than a client-reported `cli.start.kind`): it
  needs no allowlist change or SDK release, so it works for every SDK version
  already sending `cli.command.completed`. The cost is that it approximates
  cold: it catches first run and first run after an upgrade, not a cold file
  cache or a first run after reboot. A client marker is the only way to capture
  those; it can be added later as a separate dimension without changing this
  definition.
- **Cold run**: the install's first completed command ever
  (`installs.first_command_at`), or its first completed command at or after a
  recorded version change (`install_version_changes.at`, upgrades and
  downgrades). Both anchors are per install, not per command: the first run
  after an upgrade is cold whatever command it was. One run can be the first
  after several anchors; it is counted once. Everything else is **warm**.
- **Anchors that never ran**: an install with a first-ever or post-change
  anchor but no completed command since contributes no cold run. A run is
  matched to its anchor by time only, so a version change followed by a second
  one before any command makes the run after the second change the cold one
  for both.
- **Filters** (`command`, `cli_version`, `os`, `arch`, `is_ci`) choose which
  runs are reported. They never change which are cold: filtering to `build`
  does not turn a later `build` into the install's cold run. `exit_code` doesn't
  apply and `attr.*` is rejected with a 400.
- **Sample**: runs with a `cli.install_id`, a command name and a numeric
  `cli.command.duration_ms`; negative durations are dropped. Each side reports
  `run_count`, and percentiles (`percentile_agg` sketch, like `latency`) are
  absent when it is 0. Cold groups are small by construction (one run per
  install per anchor), so readers should check `run_count` first.
- **Retention**: runs come from raw logs, so results are bounded by log
  retention. A first run that aged out is not replaced by a later one: the
  anchor's timestamp is kept, but the run is gone, so no cold sample.
  Anonymous-tier runs carry no install id and are invisible
  (`/v1/metrics/install-coverage`).
- **Response**: time-bucketed (hour/day/week/month, default day), one row per
  (bucket, command) with `cold` and `warm` objects; `limit`/`offset` page the
  rows. Computed per bucket from the raw runs, so unlike `latency` there is no
  rollup to merge: wide ranges cost a scan of the logs.

### 9. Session environment

`/v1/metrics/install-coverage` says how much traffic is seen; the environment
endpoint says what that traffic runs on. `GET /v1/metrics/session-environment`
takes the distribution of one fact reported on `cli.session.started`, picked
with the required `dimension` param:

| `dimension` | Attribute | Values |
| --- | --- | --- |
| `shell` | `cli.shell` | as reported (`bash`, `zsh`, ...) |
| `terminal` | `cli.terminal` | as reported |
| `stdout_tty` | `cli.stdout.is_tty` | `tty`, `piped` |
| `runtime` | `cli.runtime.name` | as reported (`go`, `node`, ...) |
| `runtime_version` | `cli.runtime.name` + `cli.runtime.version` | `go 1.22.1` |
| `config_customization` | `cli.config.customized_count` | `0`, `1-2`, `3-5`, `6+` |
| `auth_state` | `cli.auth.state` | `authenticated`, `anonymous`, `unknown` |

- **Allowlist**: `cli.stdout.is_tty`, `cli.runtime.name`,
  `cli.runtime.version`, `cli.config.customized_count` and `cli.auth.state` are
  new, `basic` tier, non-sensitive and allowed on `cli.session.started` only
  (one per process, not per command). `cli.runtime.*` are bounded to 32 bytes;
  `cli.auth.state` is an enum, and a value outside it is dropped with the
  session kept. `cli.config.customized_count` is a count, never the settings'
  names or values. The SDKs that send them are separate work (argvio-go#6,
  argvio-go#7, argvio-node#9, argvio-node#10).
- **Why a dimension param, not an endpoint each**: they share one query shape,
  and a new fact is then one expression, not one handler, route and OpenAPI
  path.
- **Top-K and `other`**: `terminal`, `shell` and the runtime are free text a
  client can influence, so the response lists the top `limit` values, most
  sessions first (ties by value), and folds the rest into `other` (`value_count`
  distinct values, `session_count` sessions). `other` is a separate object, not
  a value, so it can't collide with a terminal that calls itself "other".
  `offset` doesn't apply. `config_customization` is bucketed server-side, so
  the number a CLI reports never appears in a result; a negative count is not
  a believable measurement and is not reported.
- **Not reported**: `session_count` is every session start in scope and
  `reported_sessions` those that carried the fact, so the difference did not:
  anonymous tier (which strips every one of these), an older SDK, or a CLI that
  can't tell. `values` and `other` add up to `reported_sessions`, never to
  `session_count`. `runtime_version` needs both attributes: a version without
  its runtime is ambiguous, and counts as not reported.
- **Unit**: process starts (`cli.session.started`), not the command bursts of
  "Sessions". A heavy user weighs by how often they start the CLI.
- **Retention, filters**: raw logs, so bounded by log retention. The
  `cli_version`, `os`, `arch` and `is_ci` filters apply; `command`
  and `exit_code` don't (a session start has no command), and `attr.*` is
  rejected with a 400. No
  migration.

## Decisions (agreed)

1. **Retention vs `installs`**: purge rows by `last_seen` against the
   tenant's retention, and treat "returning after the retention window" as
   new, and the API documents that.
2. **Tier of error category**: lower `cli.error.type` from `full` to
   `basic` in the allowlist and in both SDKs (which currently gate the
   `cli.error.raised` event at `full`). The raw message and stack stay at
   `full`/`optin_plus`.
3. **Coverage**: journey metrics need `basic`+ (`install_id`), so they cover
   consenting users only. The API exposes the share of sessions that carried an
   `install_id` (`/v1/metrics/install-coverage`).

## Order of work

1. Backend only (ships value with no SDK release): `error-types`,
   help rate and abandonment in `friction`, from existing events and exit
   codes.
2. Allowlist change plus SDK changes in both repos
   (`cli.command.completed`, ids on invoked, `WithInstallMethod`).
3. `installs` table, migration and ingest upsert; backfill from the last 90
   days of `cli.session.started`.
4. `funnel`, `activation`, `version-transitions` endpoints.

Steps 1 and 2 are independent and can be built in parallel.

## Progress

- Step 1 (partial): `/v1/metrics/error-types` and `/v1/metrics/friction`
  (help views, interrupted runs) are implemented. Repeated failure of the same
  command by one install is in `/v1/metrics/retries` (section 7).
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
  - The "repeated failure of the same command by one install" signal needs a
    per-install command history that the installs table doesn't keep. It is
    derived from raw `cli.command.completed` logs (bounded by log retention)
    in `/v1/metrics/retries`.
- Follow-up (done, unreleased): two more endpoints over install state:
  - `GET /v1/metrics/install-methods`: installs per first-seen bucket by
    vendor-reported install method (null when never reported).
  - `GET /v1/metrics/new-vs-returning`: distinct installs active per bucket,
    split by first seen in that bucket vs earlier (raw session logs joined to
    `installs`, so bounded by log retention; installs without a row are left
    out).
- Validated end to end against a local stack with both demo CLIs. The
  new-vs-returning demo data backdates five installs' first sighting, since
  the CLIs can't produce an earlier first run.
- Feature and flag discovery (#17, done, unreleased, server side only):
  - Definition: an install *discovers* a feature the first time it emits
    `cli.feature.activated` for it (named by `cli.activation.feature`), and a
    flag the first time a `cli.command.invoked` carries its name in
    `cli.flag.names`. Time to discovery is that first use minus
    `installs.first_seen`, so it is measured from when argvio first saw the
    install, not from when it was installed. A gap that comes out negative
    (a late session start) counts as zero.
  - Allowlist: `cli.feature.activated` now permits `cli.install_id` and
    `cli.session_id` (both `basic` tier, so anonymous-tier activations stay
    unlinked and are not counted). The SDKs have to start emitting the event
    with the install id (`getargvio/argvio-go#2`, `getargvio/argvio-node#5`);
    until they do, only flags appear.
  - Data source: a persisted `install_discoveries(tenant, install, kind,
    name, first_at)` table (migration 0008), upserted at ingest like
    `installs`, rather than raw logs. Logs are kept ~90 days, so an old
    install's "first" use would look late (or be missing) once the real
    first use aged out. Rows live as long as their install: the retention
    sweep deletes them with it (and stale rows whose install has no row), so
    an install returning after the retention window counts as new, with all
    its discoveries. `argvio installs backfill` rebuilds them from retained
    logs; uses older than that can't be recovered, so those installs look
    later than they were. Vendor-supplied names are bounded at 64 characters
    by the allowlist, and the endpoint returns only the top-K per kind.
  - `GET /v1/metrics/feature-discovery`: for the installs first seen in the
    window, per feature and flag (`kind` narrows it), the installs that have
    used it, how many did within 1/7/30 days of first seen, and the median
    seconds to discovery (discoverers only). `limit`/`offset` rank by installs
    per kind. Like the funnel, discoveries count whenever they happened, so
    recent cohorts are still maturing. Install-level, so basic tier and above
    only (`/v1/metrics/install-coverage`).
- Session length and command sequences (#14, done, unreleased, server side
  only): see "Sessions" below.
- Remaining from this design, tracked as issues: typo detection (#21),
  repeated-failure and retry signals (#16).
- Cold vs. warm latency (#18, done, unreleased, server side only):
  `GET /v1/metrics/latency-cold-warm`, see "Cold vs. warm latency" above. No
  migration and no SDK change: cold is derived from `installs`,
  `install_version_changes` and the existing `cli.command.duration_ms`.
- Session environment (#22, done, unreleased, server side only):
  `GET /v1/metrics/session-environment`, see "Session environment" above. The
  allowlist accepts the new attributes now; the SDK work is tracked in
  argvio-go#6/#7 and argvio-node#9/#10. No migration.
