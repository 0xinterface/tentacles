# tentacles

`tentacles` is a single-host supervisor for GitHub Actions runners. It
authenticates as a GitHub App, owns one runner scale set through
[`github.com/actions/scaleset`](https://github.com/actions/scaleset), and
starts official [`actions/runner`](https://github.com/actions/runner)
processes with just-in-time (JIT) configuration. Every process is a
one-job slot: when the job ends, the slot directory, `_work`, and all JIT
material are wiped, and the next job starts from a fresh copy of the
payload.

No Docker, no microVMs, no Kubernetes, no `config.sh`, no `svc.sh`, no
inbound webhooks.

Contents:

1. [Security warning](#security-warning)
2. [How it works](#how-it-works)
3. [Quickstart](#quickstart)
4. [Configuration](#configuration)
5. [Scaling semantics](#scaling-semantics)
6. [Payload and slot lifecycle](#payload-and-slot-lifecycle)
7. [Observability](#observability)
8. [systemd integration](#systemd-integration)
9. [Failure modes and troubleshooting](#failure-modes-and-troubleshooting)
10. [Upgrades](#upgrades)
11. [Development](#development)
12. [Known limitations](#known-limitations)
13. [Acceptance criteria](#acceptance-criteria)

## Security warning

> **This is not job isolation.** Jobs run as the `gha-runner` Unix user
> directly on the host, with host toolchains, caches, and network.
> Anyone who can queue a workflow targeting the scale set can run code
> on this machine. The control that matters lives in GitHub, not in this
> daemon: restrict the runner group to selected repositories, and only
> install the App on orgs you trust (plan §15).

If you cannot accept that model, this project is not the right tool. Use
container- or VM-isolated runners instead.

## How it works

```
                    GitHub Actions service
                  (scale set + job assignment)
                              ^
                              | App JWT / installation token
                              | session + long poll + JIT config
                              v
                     tentacles.service
                  (always on, runs no job code)
                              |
              +---------------+----------------+
              |  slot table | payload | systemd |
              +---------------+----------------+
                              |
        tentacle-0001       tentacle-0002      tentacle-000N
        run.sh --jitconfig   (one job each, ephemeral)
        User=gha-runner
```

- The scale-set listener (the upstream `listener` package) long-polls the
  message API. `statistics.TotalAssignedJobs` is the only scaling signal.
  Job lifecycle messages never change the runner count.
- A reconciler keeps `clamp(TotalAssignedJobs, min_runners, max_runners)`
  official runner processes alive, one slot per process.
- A slot is a `systemd-run` transient unit named `tentacle-<id>.service`
  that runs `./run.sh --jitconfig "$(cat /run/tentacles/<id>.jit)"` as
  `gha-runner`. The JIT value lives in a `0600` tmpfs file that the shell
  reads at start, so the secret never sits in a long-lived `argv`
  (the pinned runner line only accepts the value as an argument; see
  `docs/spike.md`).
- Workflows target the scale-set name, not `self-hosted`:

  ```yaml
  jobs:
    test:
      runs-on: debian-host
  ```

Slot states: `starting` (provisioning), `idle` (runner up, no job),
`busy` (JobStarted seen), `stopping`, `failed` (start failed). Only
`starting`, `idle`, and `busy` count toward the actual runner count, so
reconcile never overshoots `max_runners` mid-provision.

## Quickstart

### 1. GitHub App

1. Create a GitHub App (org or repo scope). Record the client id and
   installation id; save the private key PEM.
2. Grant the scale-set permission:
   - Organization scale set: `organization_self_hosted_runners`
   - Repository scale set: `administration`
3. Install the App on the org (or repo) that owns the scale set.
4. Restrict the runner group to selected repositories only. This is the
   mitigation for the security warning above.

### 2. Host bootstrap (one-time, as root)

```sh
sudo ./scripts/install-host-deps.sh
```

The script installs the runner's library dependencies for the pinned
2.328 line (`libicu` with distro detection, `libkrb5-3`, `zlib1g`) plus
`curl`, `ca-certificates`, `jq`, and `git`. It is idempotent.

Then, per plan §18:

- Create users: `tentacles` (daemon) and `gha-runner` (jobs, with a
  home directory). The daemon user must not be writable by `gha-runner`.
  `gha-runner` gets no sudo and no Docker socket.
- Create directories: `/etc/tentacles`, `/var/lib/tentacles`,
  `/var/cache/tentacles`, `/var/log/tentacles`, `/run/tentacles`
  (tmpfs for JIT files).
- Install `mise` and the toolchains jobs need, as `gha-runner`, and put
  the shims on `PATH` in `/etc/tentacles/runner.env` (see
  [Configuration](#configuration)). Jobs do not source `.bashrc`.
- Optionally run the payload's own
  `bin/installdependencies.sh` once per host/version; the script above
  covers the same base dependencies for the pinned line.

### 3. Build and configure

Go 1.26+ (see `go.mod`):

```sh
go build -ldflags "-X github.com/hkust/tentacles/internal/version.Version=v1.0.0" \
    -o tentacles ./cmd/tentacles
sudo install -m 0755 tentacles /usr/local/sbin/tentacles
sudo install -m 0600 app.pem /etc/tentacles/app.pem
sudo cp configs/config.example.yaml /etc/tentacles/config.yaml
# edit /etc/tentacles/config.yaml: App creds, scope, capacity, runner pin
```

Validate without starting anything:

```sh
tentacles --config /etc/tentacles/config.yaml --dry-run   # exit 0 = valid
```

### 4. Enable the service

```sh
sudo install -m 0644 configs/systemd/tentacles.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tentacles
```

The unit is `Type=notify`; systemd considers it started only after the
listener session exists, so a broken App or network fails loudly instead
of reporting a healthy daemon.

Confirm the scale set appears under org, Settings, Actions, Runners.
Then move one repository to `runs-on: debian-host` while any existing
standing runner is still up. Retire the old `svc.sh` unit only after the
first real job passes. Never run two agents against the same work
directory (plan §18).

## Configuration

The binary takes three flags:

| Flag | Meaning |
|---|---|
| `--config <path>` | config file, default `/etc/tentacles/config.yaml` |
| `--dry-run` | load and validate the config, exit 0 |
| `--version` | print the build version |

### Fields and defaults

Omitted fields fall back to these defaults (plan §6). The decoder is
strict: an unknown key is a startup error, not a silent no-op.

| Field | Default | Notes |
|---|---|---|
| `github.url` | `https://github.com` | GHES roots work; scope is appended |
| `github.app.client_id` | required | |
| `github.app.installation_id` | required, `> 0` | |
| `github.app.private_key_path` | required | PEM file, read at startup |
| `github.scope.kind` | required | `organization` or `repository` |
| `github.scope.owner` | required | |
| `github.scope.repository` | required when kind is `repository` | |
| `scale_set.name` | required | valid Actions label; this is `runs-on` |
| `scale_set.runner_group` | `Default` | |
| `scale_set.extra_labels` | `[]` | |
| `capacity.min_runners` | `0` | warm pool size |
| `capacity.max_runners` | required | hard cap 32 |
| `capacity.job_cpu_quota_percent` | required | systemd `CPUQuota`, e.g. 400 |
| `capacity.job_memory_max` | required | e.g. `8G` |
| `runner.version` | unset | `X.Y.Z` pins the payload; unset tracks the latest release |
| `runner.download_url` | official release URL | override for mirrors |
| `runner.sha256` | unset (required when `version` is pinned) | unless `TENTACLES_ALLOW_UNVERIFIED_PAYLOAD=1` |
| `runner.work_directory` | `_work` | JIT work folder inside the slot |
| `runner.disable_update` | `true` | runner self-update off at scale-set creation |
| `runner.user` | `gha-runner` | |
| `runner.group` | same as user | |
| `runner.environment_file` | required | systemd EnvironmentFile path |
| `paths.state_dir` | `/var/lib/tentacles` | slots + template live here |
| `paths.cache_dir` | `/var/cache/tentacles` | payload tarball cache |
| `paths.log_dir` | `/var/log/tentacles` | shipped `_diag` archives |
| `runtime.backend` | `systemd` | `process` for dev hosts without systemd |
| `runtime.slot_start_timeout` | `90s` | bounds each backend start |
| `runtime.slot_stop_timeout` | `30s` | bounds each stop (also `TimeoutStopSec`) |
| `runtime.cleanup_timeout` | `60s` | bounds slot wipe |
| `runtime.acquire_grace` | `3m` | see [Scaling semantics](#scaling-semantics) |
| `runtime.jit_dir` | `/run/tentacles` | tmpfs in production |
| `observability.listen` | `127.0.0.1:9090` | metrics + `/healthz` |
| `observability.log_level` | `info` | `debug`, `warn`, `error` also accepted |
| `observability.ship_diag` | `true` | copy `_diag` before wipe |
| `scaling.admission_control` | `true` | history-based admission gate |
| `scaling.cpu_target_percent` | `90` | share of host cores busy jobs may use |
| `scaling.memory_margin_percent` | `20` | share of MemAvailable kept free |
| `scaling.sample_interval` | `30s` | usage sampling period |

Validation fails closed and reports every problem at once: `max_runners
>= 1`, `min_runners <= max_runners`, the 32-slot hard cap, readable PEM,
positive installation id, valid scale-set label, sha256 digest present
unless the env escape is set, environment file present and parseable
with `PATH` and `HOME` non-empty, systemd backend only when
`/run/systemd/system` exists, and every state directory writable
(probed by creating a file, not just `mkdir`).

### runner.env

`runner.env` is a systemd `EnvironmentFile`, not YAML:

```dotenv
PATH=/home/gha-runner/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
HOME=/home/gha-runner
MISE_DATA_DIR=/home/gha-runner/.local/share/mise
LANG=C.UTF-8
```

The daemon parses it at startup and refuses to run jobs if `PATH` or
`HOME` is missing. This is the fix for the classic "systemd does not see
mise/node" failure mode (plan §12). Jobs are not login shells; if a tool
only works after `eval "$(mise activate bash)"`, fix this file instead.

## Scaling semantics

- Desired = `statistics.TotalAssignedJobs`, then
  `desired = clamp(desired, min_runners, max_runners)`. Nothing else
  moves the count. Canceled and reassigned jobs are ignored on purpose;
  statistics already reflect current truth.
- `min_runners: 1` keeps one idle `run.sh` registered as a warm pool.
  That is the latency knob; the default `0` scales to zero processes and
  zero slot directories when idle.
- Starts are sequential in v1. A slot being provisioned counts toward
  actual, so reconcile cannot overshoot the cap.
- Scale-down stops the oldest `idle` slots first. `starting` slots
  become eligible once they pass the acquire grace. Busy slots are
  never stopped, no matter how far desired drops.
- Reconcile runs on three triggers: a new desired count from the
  listener, a slot exit (the process-exit watcher), and a 30 second
  tick. The tick is plan §13's "retry next tick": a failed JIT call or a
  disk-watermark rejection is retried without waiting for GitHub.
- **Admission control** (on by default): every job's real usage is
  measured from systemd accounting (CPU seconds, peak memory) at slot
  exit, keyed by workflow ref, and kept as a per-ref moving average in
  `state_dir/history.jsonl`. Before starting a slot the gate predicts
  the cost of all busy jobs plus the incoming one; over budget (CPU
  target share of host cores, memory under `MemAvailable` minus margin)
  the start is held as backpressure and retried on the next tick. Queued
  jobs are identified from `JobAvailable` messages, so the prediction
  uses that workflow's own history when available. The gate stays inert
  until history exists, always admits the first job on an idle host, and
  does nothing without systemd accounting (process backend).
- Exit classification (plan §13): a runner that exits before any
  JobStarted and inside the acquire grace is an acquire failure
  (metric + event, slot wiped). A runner that exits after a job is a
  plain exit. An idle warm-pool runner is never torn down for being
  idle; only a surplus decision from reconcile stops it.
- The listener restarts with 1s, 2s, 5s, 15s, 30s backoff on failure.
  Existing runners keep working during a reconnect.

## Payload and slot lifecycle

On start (and on a `runner.version` or sha change):

The version comes from one of two modes:

- **Pinned** (recommended for production): `runner.version` plus
  `runner.sha256` are set. The daemon downloads exactly those bytes or
  fails.
- **Tracking** (`runner.version` unset): the daemon queries the GitHub
  releases API for the latest `actions/runner` release, reads the
  version from the tag and the sha256 from the release asset's digest,
  and verifies the download against it. This still fails closed: if the
  API is unreachable or the release carries no digest, startup fails
  and tells you to pin the version instead. Unauthenticated GitHub API
  calls are rate limited (60/hour per IP), and each daemon start makes
  exactly one; pin the version in production.

Then, in both modes:

1. Download `actions-runner-linux-x64-<ver>.tar.gz` into
   `paths.cache_dir` if not cached. Downloads are capped at 1 GiB.
2. Verify the sha256 from config. A mismatch triggers one redownload,
   then a hard failure; the daemon refuses to start any slot.
3. Extract into `paths.state_dir/template` atomically: staging
   `template.tmp`, fsync of every file, fsync of the marker, rename.
   The template carries a version+sha marker, so an unchanged payload is
   skipped entirely.
4. Never run `config.sh` on the template.

Per slot:

1. Copy the template to `slots/<id>` (`cp -a` semantics; the template
   assumes exclusive ownership of its install directory, so slots are
   never shared).
2. Mint a JIT config named `<scale-set>-<id>-<rand>` with the work
   folder set to the slot's `_work`, write it `0600` to `jit_dir`,
   start the unit.
3. On exit: copy `_diag` to `log_dir/<id>-<utc>-<runnerName>` (if
   `ship_diag`), shred and remove `.runner`/`.credentials` files, unlink
   the JIT file, delete the slot directory. The next slot re-copies the
   template.

Before starting a slot the daemon checks the state filesystem: under 10%
free it refuses new slots (metric + log) and retries on the next tick,
keeping the listener up so GitHub sees the real capacity (plan §16).

## Observability

Logs are structured JSON via `log/slog` on stderr, so journald picks
them up: `journalctl -u tentacles -o json`. Fields: `scale_set`,
`slot`, `runner_name`, `desired`, `actual`, `event`, `err`. The JIT
payload, the PEM, and installation tokens are never logged.

The metrics listener (default `127.0.0.1:9090`) serves Prometheus text
format plus a liveness endpoint at `/healthz`:

| Metric | Meaning |
|---|---|
| `tentacles_desired_runners` | last clamped desired count |
| `tentacles_actual_runners{state=}` | slots per state (`starting`, `idle`, `busy`, `stopping`, `failed`, `empty`) |
| `tentacles_jobs_started_total` | JobStarted messages seen |
| `tentacles_jobs_completed_total{result=}` | JobCompleted by result (`success`, `failure`, `canceled`, ...) |
| `tentacles_acquire_failures_total` | failed starts + never-claimed exits |
| `tentacles_slot_start_seconds` | histogram of full provision time |
| `tentacles_listener_errors_total` | listener/session failures |
| `tentacles_last_message_id` | last scale-set message ID processed |
| `tentacles_job_cpu_seconds` / `tentacles_job_wall_seconds` | finished-job histograms |
| `tentacles_last_job_peak_memory_bytes` | peak memory of the last job |
| `tentacles_admission_holds_total` | starts held by the admission gate |

`_diag` shipping is on by default and should stay on: without it, a
failed ephemeral job leaves no runner-side logs anywhere.

## systemd integration

- The daemon unit is `Type=notify`. `READY=1` goes out only after: config
  validated, payload verified and extracted, GitHub client created,
  scale set ensured, boot adoption done, and the listener session
  created (plan §11). If the session never comes up, the daemon never
  reports ready; systemd restarts it until the network or credentials
  are fixed.
- Slot units are transient: the daemon execs
  `systemd-run --unit=tentacle-<id>` with `-p` properties from config
  (`User`, `Group`, `CPUQuota`, `MemoryMax`, `EnvironmentFile`,
  `TimeoutStopSec`, hardening). There are no root-owned drop-in files,
  and tests inject fake binaries via `PATH`.
  `configs/systemd/tentacle@.service` is a static equivalent for
  operators who prefer template units.
- Shared caches survive the hardening. `ProtectHome=read-only` would
  otherwise make `mise` and Go module caches read-only, which breaks
  real jobs. The daemon adds the runner user's `~/.cache`,
  `~/.local/share/mise`, and `~/go/pkg/mod` to `ReadWritePaths`,
  creating those directories first (as the runner user when the daemon
  runs as root) so they can never end up root-owned. The static unit
  uses `%h` specifiers for the same effect.
- Slot control is deliberately narrow: the daemon needs the ability to
  manage `tentacle-*.service` only. Use a polkit rule limited to
  `org.freedesktop.systemd1` unit operations on that name pattern, or a
  systemd user transient scope. Running the daemon as root with the
  unit hardening (`NoNewPrivileges`, `ProtectSystem=strict`) is
  documented technical debt, not the goal.
- Boot adoption (plan §13): on start the daemon lists
  `tentacle-*.service` units and `slots/*` directories. A running unit
  with a matching directory is adopted as busy and its runner name is
  recovered from the slot's `.runner` file, so an in-flight job keeps
  correlating with `JobStarted`. A directory without a unit is wiped. A
  unit without a directory is stopped. Only then does the listener
  start. A daemon restart never SIGKILLs a running job.
- Graceful shutdown: SIGTERM stops idle and starting slots, lets busy
  runners finish (one-job runners exit on their own; anything left is
  recovered by boot adoption), and deletes the message session. The
  scale set itself is never deleted.

## Failure modes and troubleshooting

| Symptom | Detection | What happens |
|---|---|---|
| Listener 401/403 | `tentacles_listener_errors_total` climbs | session fails, backoff, retry. Do not flap scale-set creation |
| Session drop | listener error | reconnect with backoff; runners keep working |
| JIT generate fails | acquire failure metric, log | desired stays unsatisfied; retried on the next tick |
| `run.sh` exits with no job, fast | acquire failure metric | slot wiped; GitHub reassigns the job (up to 3 attempts) |
| Start hangs | `slot_start_timeout` (90s) | stop, wipe, retry next tick |
| Job canceled | `jobs_completed_total{result="canceled"}` | no scaling action; statistics drive desired |
| Daemon restart mid-job | boot adoption log lines | running unit adopted as busy, job untouched |
| Disk nearly full | watermark log/metric, `actual` stays flat | no new slots until 10% free again; listener stays up |
| Host reboot | leftover dirs wiped at boot | GitHub times out or requeues the job |
| Payload sha mismatch | startup error | no slots start; fix `runner.sha256` or the mirror |
| `mise`/PATH broken in jobs | first job fails | operational: fix `runner.env`; keep a canary workflow |

Debugging recipes:

- A slot died strangely: `ls /var/log/tentacles/` for the `_diag`
  archive of that run, and `journalctl -u tentacles --since -1h`.
- Warm pool missing: check `tentacles_actual_runners{state="idle"}`,
  then the acquire-failure counter. A high failure rate usually means a
  bad `runner.env` or a blocked egress path.
- Suspected leak: `systemctl list-units 'tentacle-*'` and
  `ls /var/lib/tentacles/slots/`. Boot adoption reconciles any
  mismatch on the next restart; a running daemon reconciles via
  desired counts.

## Upgrades

- **Daemon**: build the new binary, `systemctl restart tentacles`.
  Boot adoption recovers in-flight jobs; idle slots are stopped and
  re-created. There is no state to migrate.
- **Runner version**: bump `runner.version` (and `sha256` if pinned) and
  restart. The next payload ensure downloads the new tarball, extracts a
  fresh template, and swaps it atomically; slots created after that use
  the new version. Jobs in flight keep the old tree until they exit.
  Runner dependencies occasionally change; re-run
  `scripts/install-host-deps.sh` (it sets `RUNNER_VERSION` if you need a
  different informational target) or the payload's own
  `bin/installdependencies.sh`.
- **`actions/scaleset`**: pinned at v0.4.0 because it is a public
  preview API. Read the upstream changelog before bumping; the adapter
  in `internal/scaleset` is the only place that compiles against it, so
  interface drift is contained.

## Development

```
tentacles/
  cmd/tentacles/        entry point: flags, signals; no business logic
  internal/
    config/              YAML load (strict), validation, dirs
    app/                 wiring and the run loop
    scaleset/            the only importer of actions/scaleset
    reconcile/           desired vs actual
    slot/                slot table: allocate, start, stop, wipe, adopt
    payload/             download, verify, extract the runner tarball
    runner/              JIT write, exec spec, backend contract
    systemd/             transient-unit backend, sd_notify
    process/             plain-child backend for tests and dev hosts
    env/                 EnvironmentFile parsing
    cleanup/             credential shredding
    logship/             _diag copy before wipe
    metrics/             hand-rolled Prometheus exposition
    version/             build-time version string
  configs/               example config, systemd units
  scripts/               host dependency bootstrap
  testdata/              dry-run PEM, fake runner
```

Rules that hold the design together:

- `internal/scaleset` is the only package importing
  `github.com/actions/scaleset`. Upstream types do not leak past it.
- Provisioning is an interface (`runner.Backend`). Production is the
  systemd backend; tests use the process backend plus
  `testdata/fake-runner/run.sh`, a stub agent that fails without a JIT
  value, writes a claimed marker, and exits after `FAKE_RUNNER_SLEEP`
  seconds with `FAKE_RUNNER_EXIT_CODE`.
- The systemd backend execs `systemd-run`/`systemctl`; tests inject fake
  binaries via `PATH`, so the whole daemon tests on macOS too.

Testing:

```sh
go build ./...
go vet ./...
gofmt -l .
go test -race ./...
go run ./cmd/tentacles --config configs/config.example.yaml --dry-run
```

The app tests include an end-to-end daemon run with a fake scale set,
the process backend, and the fake runner: scale up from a queued job,
JIT delivery, `_diag` shipping, scale-down to zero, warm-pool
replenishment after a runner dies, and readiness gating. CI runs the
same gates on Ubuntu and macOS with Actions pinned to full SHAs
(`.github/workflows/ci.yml`).

What offline tests cannot prove: the live integration checklist (plan
§17) against a real App and a disposable repo, and the phase-6 soak
(24h of starts, reconnects, and a forced `kill -9` of one `run.sh`
without leaks or double registration). Run both before trusting the
host with real workloads.

## Known limitations

- **No isolation.** The warning at the top is the contract. GitHub's
  runner-group restriction is the only access control.
- **Single host, single scale set, one App installation.** This is not a
  multi-host scheduler.
- **Preview client pin.** `actions/scaleset` v0.4.0 is a preview API;
  the pin is deliberate and the adapter absorbs drift.
- **JIT value crosses `ps`.** The pinned 2.328 runner only accepts
  `--jitconfig <value>`, so the shell exposes the secret for
  milliseconds while reading it from the `0600` tmpfs file. Acceptable
  on a trusted host; revisit if upstream ships `--jitconfig-file`.
- **One job per process.** The slot tree is destroyed after every job.
  There is no reuse, by design.
- **Shared caches are a residue channel.** `~/.cache`, mise state, and
  `~/go/pkg/mod` are shared across jobs because that is the point of a
  shared host. Accepted under the trusted-org constraint.
- **Sequential starts.** Slot creation is serial in v1; a cold scale-up
  of N slots takes roughly N times one provision. Parallel starts are a
  listed future optimization (plan §7 phase 7).

## Acceptance criteria

Mapped to plan §19. Items 1 to 4 are design properties; 5 to 10 are
verified by tests or pending live verification.

1. A Debian host with only `tentacles.service` persistent executes
   `runs-on: debian-host` workflows. (Quickstart; needs live check.)
2. Authentication is a GitHub App; no PAT in config or docs.
3. No Docker, no microVM, no Kubernetes at runtime.
4. No `config.sh` / `svc.sh` anywhere in the path.
5. `min_runners: 0` idle means zero `run.sh` processes and zero slot
   directories. (Tested.)
6. Concurrent jobs up to `max_runners` each get their own slot directory
   and unit. (Tested.)
7. After every job, `_work`, JIT, and the slot tree are gone; `_diag` is
   in `log_dir`. (Tested, including credential shredding.)
8. `mise`-installed `node` is visible to jobs via `runner.env`, and
   shared caches are writable under the systemd sandbox. (Cache paths
   unit-tested; end-to-end `node -v` pending live check.)
9. Listener and daemon restarts leak no scale sets or runners. (Boot
   adoption tested offline; live restart pending.)
10. The isolation limitation and runner-group control are documented.
    (This file.)

Design rationale and per-phase exit criteria live in
`tentacles-implementation-plan.md`; upstream API findings are in
`docs/spike.md`.
