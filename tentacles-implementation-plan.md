# tentacles Implementation Plan

Single-host GitHub Actions runner supervisor for Debian.

The daemon authenticates as a GitHub App, owns one runner scale set via `github.com/actions/scaleset`, and starts official `actions/runner` processes with JIT configuration. It does not use Docker, microVMs, Kubernetes, `config.sh`, `svc.sh`, or `workflow_job` webhooks.

Status of the upstream client: public preview. Pin a release (currently `v0.4.0`) and treat interface drift as a first-class risk.

---

## 1. Goal

Ship a long-running Go daemon, `tentacles`, that:

1. Registers or reuses one GitHub Actions runner scale set at org or repo scope.
2. Long-polls the scale-set message API and treats `statistics.TotalAssignedJobs` as the only scaling signal.
3. Keeps `min(max(desired, min_runners), max_capacity)` official runner processes alive on the host.
4. Starts each runner from the official Linux tarball with a one-job JIT config.
5. Tears the slot down after the process exits: work dir, diagnostics, JIT material.
6. Scales to zero when idle if `min_runners: 0`.

Workflows target the scale-set name, not `self-hosted`:

```yaml
jobs:
  test:
    runs-on: debian-host
```

---

## 2. Non-goals

- Container, LXC/Incus, Firecracker, or VM isolation.
- Multi-host scheduling or a control plane talking to other machines.
- Persistent `config.sh` registration or wrapping `svc.sh`.
- Inbound webhook listener.
- Untrusted PR isolation. Jobs share the host kernel, user, network, and installed toolchains.
- Acting as a general GitHub App platform. One installation, one scale set, one host.

If a later requirement is “untrusted code”, this design is the wrong product. Stop and change the provisioner, do not bolt isolation onto process slots.

---

## 3. Constraints

| Constraint | Implication |
|---|---|
| Single Debian host | Capacity is CPU, RAM, disk, and FD limits. `max_capacity` must match that, not a wish. |
| GitHub App only | No PAT path in the first release. Credentials are `client_id`, `installation_id`, PEM. |
| Official runner payload | Reuse the tarball and `run.sh`. Do not reimplement the agent. |
| No Docker / no microVM | Provisioner is `exec` + systemd transient units. |
| No `config.sh` / `svc.sh` | JIT replaces registration. Only `tentacles.service` is persistent. |
| Host-native tools | Jobs see the host toolchain. `PATH` / `mise` must be injected explicitly. |
| Ephemeral by default | One job per process. Reset the slot filesystem after every exit. |
| Trusted org / group runners | Sandbox for blast-radius reduction, not a security boundary. |
| Scale-set client is preview | Pin the module. Hide it behind a thin adapter so upgrades are local. |

GitHub App permissions (from GitHub autoscaling docs):

- Organization scale set: `organization_self_hosted_runners`
- Repository scale set: `administration`

No inbound ports. The host only needs egress to `github.com` / `*.actions.githubusercontent.com` (and GHES, if used).

---

## 4. Product shape

```text
                    GitHub Actions service
                    (scale set + job assign)
                              ^
                              | App JWT / installation token
                              | session + long poll + JIT
                              v
                     tentacles.service
                     (always on, no job code)
                              |
              +---------------+----------------+
              | slot alloc | payload | systemd |
              +---------------+----------------+
                              |
           tentacle@0001     tentacle@0002     tentacle@000N
           run.sh --jitconfig
           User=gha-runner
```

Operator-facing artifacts:

- Binary: `/usr/local/sbin/tentacles` (or `/opt/tentacles/tentacles`)
- Config: `/etc/tentacles/config.yaml`
- App key: `/etc/tentacles/app.pem` (`0600`, root or `tentacles`)
- Runner env: `/etc/tentacles/runner.env` (`PATH`, `mise`, caches)
- State: `/var/lib/tentacles/`
- Cache: `/var/cache/tentacles/actions-runner-linux-x64-<ver>.tar.gz`
- Logs: journald + optional copy of `_diag` to `/var/log/tentacles/`

Unix users:

- `tentacles` — daemon only. Can start/stop slot units, cannot write workflow work dirs as itself if avoidable.
- `gha-runner` — runs `run.sh` and all job steps. No sudo. No Docker socket.

If that split is too heavy for v1, run both as `gha-runner` but keep the unit hardening. Prefer the split.

---

## 5. Repository layout

```text
tentacles/
  cmd/tentacles/main.go          # process entry, signal handling
  internal/
    config/                       # load/validate YAML
    app/                          # wiring, run loop
    scaleset/                     # adapter over github.com/actions/scaleset
    reconcile/                    # desired vs actual
    slot/                         # allocate, occupy, release
    payload/                      # download, verify, extract runner tarball
    runner/                       # write JIT, build exec spec
    systemd/                      # transient unit backend
    process/                      # fallback exec backend for tests/dev
    env/                          # runner.env + mise PATH
    cleanup/                      # wipe slot after exit
    logship/                      # copy _diag before wipe
    metrics/                      # Prometheus or expvar
    version/                      # binary + pinned runner version
  configs/
    config.example.yaml
    systemd/tentacles.service
    systemd/tentacle@.service
  scripts/
    install-host-deps.sh          # runner installdependencies.sh once
    package-deb.sh                # optional later
  testdata/
    fake-runner/run.sh            # stub agent for unit/integration tests
  go.mod
  README.md
```

Rules:

- `internal/scaleset` is the only package that imports `github.com/actions/scaleset`.
- Provisioning is an interface. Production backend is systemd. Tests use the process backend plus `testdata/fake-runner`.
- No business logic in `main.go`.

Go version: **1.25+**, required by `actions/scaleset`.

Module pin:

```text
github.com/actions/scaleset v0.4.0
```

Revisit on each upstream release. Read their changelog before bumping.

---

## 6. Configuration

`/etc/tentacles/config.yaml`:

```yaml
github:
  url: https://github.com
  app:
    client_id: Iv1.xxxxxxxx
    installation_id: 12345678
    private_key_path: /etc/tentacles/app.pem
  scope:
    kind: organization          # organization | repository
    owner: my-org
    repository: ""              # required if kind=repository

scale_set:
  name: debian-host             # this is runs-on
  runner_group: Default
  extra_labels: []              # github.com cloud: usually just the name

capacity:
  min_runners: 0
  max_runners: 4
  job_cpu_quota_percent: 400    # systemd CPUQuota
  job_memory_max: 8G

runner:
  version: 2.328.0              # pin; do not float
  download_url: ""              # optional override; default GitHub release
  sha256: ""                    # required in production
  work_directory: _work
  disable_update: true
  user: gha-runner
  environment_file: /etc/tentacles/runner.env

paths:
  state_dir: /var/lib/tentacles
  cache_dir: /var/cache/tentacles
  log_dir: /var/log/tentacles

runtime:
  backend: systemd              # systemd | process
  slot_start_timeout: 90s
  slot_stop_timeout: 30s
  cleanup_timeout: 60s
  acquire_grace: 3m             # wait for job claim before treating as failed

observability:
  listen: 127.0.0.1:9090
  log_level: info
  ship_diag: true
```

Validation on boot, fail closed:

- `max_runners >= 1` and `min_runners <= max_runners`
- `max_runners` not greater than a compiled or config hard cap (suggest 32)
- PEM readable, installation id > 0
- `scale_set.name` is a valid Actions label (no spaces)
- `runner.sha256` set unless `TENTACLES_ALLOW_UNVERIFIED_PAYLOAD=1`
- `environment_file` exists
- `backend=systemd` only if `/run/systemd/system` exists
- State/cache directories writable

`runner.env` is not YAML. It is a systemd `EnvironmentFile`:

```dotenv
PATH=/home/gha-runner/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
HOME=/home/gha-runner
MISE_DATA_DIR=/home/gha-runner/.local/share/mise
LANG=C.UTF-8
```

The daemon must not start jobs until this file is present. This is the fix for the existing “systemd does not see `mise` / `node`” failure mode. Do not source `.bashrc`.

---

## 7. Core types

```go
package slot

type ID string

type State string

const (
    StateEmpty     State = "empty"
    StateStarting  State = "starting"
    StateIdle      State = "idle"      // run.sh up, not yet busy
    StateBusy      State = "busy"      // JobStarted seen, or process running after claim
    StateStopping  State = "stopping"
    StateFailed    State = "failed"
)

type Slot struct {
    ID        ID
    Dir       string
    Unit      string
    RunnerName string
    State     State
    StartedAt time.Time
    JITPath   string
}

type Manager interface {
    Desired() int
    Active() []Slot
    Ensure(ctx context.Context, desired int) error
    MarkBusy(id ID)
    ObserveExit(id ID, err error)
}
```

```go
package runner

type JIT struct {
    Encoded string // treat as secret
}

type Spec struct {
    SlotDir    string
    WorkDir    string
    JITPath    string
    EnvFile    string
    User       string
    CPUQuota   string
    MemoryMax  string
    UnitName   string
}

type Backend interface {
    Start(ctx context.Context, spec Spec) error
    Stop(ctx context.Context, unit string) error
    Wait(ctx context.Context, unit string) error
}
```

Keep slot IDs stable and short: `0001` … `00NN`. Unit names: `tentacle@0001.service`.

---

## 8. Scale-set adapter

Package `internal/scaleset` wraps the upstream client. Do not leak upstream types into `app` / `reconcile`.

Responsibilities:

1. Build a client from GitHub App credentials (`ClientID`, `InstallationID`, `PrivateKey`). Prefer App auth; the upstream client refreshes installation tokens itself.
2. Resolve the existing scale set by name, or create it.
3. Run the official `listener` loop (preferred over a hand-rolled `GetMessage` loop).
4. Translate listener callbacks into local events.
5. Mint JIT configs through `GenerateJitRunnerConfig`.
6. Always pass `maxCapacity` (`capacity.max_runners`) on poll (`X-ScaleSetMaxCapacity`).

Event surface the rest of the daemon consumes:

```go
type Events struct {
    SessionStarted func()            // after the message session is created; gates sd_notify READY
    Desired        func(n int)       // from statistics.TotalAssignedJobs
    JobStart       func(runnerName string)
    JobEnd         func(runnerName, result string)
    MessageID      func(id int64)    // every fetched message; feeds tentacles_last_message_id
    Session        func(err error)   // session drop / auth refresh failure
}
```

Scaling rules, copied from upstream guidance and non-negotiable:

- Desired runners = `statistics.TotalAssignedJobs`.
- Then apply `desired = clamp(desired, min_runners, max_runners)`.
- Do **not** increment/decrement from `JobAssigned` / `JobStarted` / `JobCompleted` counts.
- Message bodies cap at 50 items and can truncate a backlog.
- `JobAssigned` then `JobCompleted` with `result: canceled` can repeat up to 3 times when a runner fails to acquire in time. That is the same workflow job. Statistics already reflect current truth.
- After handling a message, acknowledge it (`DeleteMessage`). Unacked messages redeliver. Process first, ack second, but make `Ensure` idempotent so redelivery is safe.
- `GetMessage` long-polls ~50s and returns `nil, nil` on 202. Loop immediately.

Use job lifecycle messages only to:

- mark a slot busy so scale-down will not kill it
- emit metrics (`jobs_started`, `jobs_completed`, `jobs_canceled`)
- correlate `runnerName` with a slot

Session handling:

- One session per process.
- On listener crash, restart the session with backoff (1s, 2s, 5s, 15s, 30s cap).
- Do not delete the scale set on daemon restart.
- Do not create a second scale set with the same name. List, then reuse.

JIT generation:

- Name: `debian-host-<slot>-<short-rand>` so GitHub UI and `JobStarted` can be mapped.
- Work folder: the slot’s `_work`.
- Treat the encoded config as a secret. Write `0600` to `/run/tentacles/<slot>.jit` (tmpfs), never to the slot tree if it is world-readable.
- Unlink the JIT file once `run.sh` has started, or at latest when the slot is wiped.
- Never log the encoded value.

If the upstream JIT API lets you disable runner self-update, do it. Otherwise pin the tarball and accept that a freshly extracted payload may still try to update. Track this as an implementation spike in phase 1.

---

## 9. Reconcile loop

Single-threaded reconcile. The listener pushes `desired`. A local watcher pushes process exits. Both enqueue “reconcile now”.

```text
desired = clamp(TotalAssignedJobs, min_runners, max_runners)
actual  = count(slots in starting|idle|busy)

if actual < desired:
    start (desired - actual) slots, sequentially at first
if actual > desired:
    stop only slots in idle or starting-past-grace
    never stop busy slots
```

Sequential start for v1. Parallel start is a later optimization; JIT + extract + systemd is usually seconds, not the bottleneck.

Scale-down policy:

- Prefer oldest idle slot.
- If only busy slots exist, wait. Do not SIGKILL a job because desired dropped.
- After process exit, the slot is not reusable until cleanup finishes.

Warm pool: `min_runners: 1` keeps one `run.sh` registered and idle. That is the latency knob. Default is `0`.

Startup race: a slot in `starting` counts toward `actual` so the reconciler does not overshoot `max_runners`.

---

## 10. Official runner payload

Do not install the runner the way GitHub’s Linux UI describes (`config.sh` + `svc.sh`). Use the tarball as a cacheable payload.

On daemon start, and on config change of `runner.version`:

1. If `/var/cache/tentacles/actions-runner-linux-x64-<ver>.tar.gz` is missing, download from the `actions/runner` release that matches `runner.version`.
2. Verify SHA-256.
3. Extract to `/var/lib/tentacles/template/` (replace atomically: extract to `template.tmp`, fsync, rename).
4. Never run `config.sh` on the template.
5. Run `bin/installdependencies.sh` once per host/version, as root, from a oneshot install unit or packaging postinst. Not from the daemon if the daemon is unprivileged.

Slot materialization:

```text
/var/lib/tentacles/template/          # read-only after extract
/var/lib/tentacles/slots/0001/        # full copy or reflink
/var/lib/tentacles/slots/0002/
```

v1: `cp -a` the template into the slot. On filesystems with CoW (btrfs/xfs reflink), use `cp --reflink=auto` to make slot create cheap.

Do not share one extracted tree across slots. The official agent assumes exclusive ownership of its install directory.

Each slot contains at least:

```text
slots/0001/
  run.sh
  bin/
  _work/          # created at start, wiped at end
  _diag/          # shipped then wiped
```

`.runner` / `.credentials` must not exist before start. JIT creates the equivalent in memory / as directed by `run.sh --jitconfig`.

After every exit:

1. Stop the unit if still loaded.
2. Copy `_diag` to `/var/log/tentacles/<slot>-<utc>-<runnerName>/` if `ship_diag` is true.
3. Shred/unlink any leftover JIT or credential files.
4. `rm -rf` the entire slot directory.
5. Next use re-copies the template.

That is what “ephemeral on a shared host” actually means. Leaving `_work` around is a failed implementation.

Host-level one-time deps (from official `installdependencies.sh`): libicu, libkrb5, zlib, etc. Document them; do not download random `.deb`s from the daemon.

---

## 11. Starting a runner

Canonical exec:

```bash
./run.sh --jitconfig "$(cat /run/tentacles/0001.jit)"
```

Do not pass the JIT on the Unix command line if it shows up in `ps`. Prefer, in order:

1. A flag that reads from a file, if the installed runner version supports it.
2. An environment variable the official agent already understands, if confirmed against that version.
3. `--jitconfig` with the value only if 1 and 2 are unavailable; then immediately unlink the file and accept the `ps` exposure on a trusted host.

Phase 0 spike must settle this against the pinned runner version. Do not guess in production code. The Cloudflare and other JIT users invoke `./run.sh --jitconfig "${JIT_CONFIG}"`; verify file/env alternatives before copying that.

Systemd unit (`tentacle@.service`), instantiated as `tentacle@0001`:

```ini
[Unit]
Description=GitHub Actions runner slot %i
StopWhenUnneeded=no

[Service]
Type=exec
User=gha-runner
Group=gha-runner
WorkingDirectory=/var/lib/tentacles/slots/%i
EnvironmentFile=/etc/tentacles/runner.env
ExecStart=/var/lib/tentacles/slots/%i/run.sh --jitconfig-file /run/tentacles/%i.jit
KillMode=mixed
TimeoutStopSec=30
Nice=5
CPUQuota=400%
MemoryMax=8G
TasksMax=4096
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
# Plan §12: shared caches (~/.cache, mise, go/pkg/mod) stay writable so
# jobs can use the host toolchain. The daemon creates these as the
# runner user before the first slot unit starts.
ReadWritePaths=/var/lib/tentacles/slots/%i /tmp %h/.cache %h/.local/share/mise %h/go/pkg/mod
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
LockPersonality=yes
```

Adjust `ExecStart` to whatever the spike confirms. The daemon should `systemd-run` or `StartTransientUnit` with these properties so values come from config, not a static unit, if quotas must be config-driven. A static `@.service` plus `systemctl start tentacle@0001` is simpler for v1 if quotas are fixed.

The daemon’s own unit:

```ini
[Unit]
Description=GitHub Actions host runner supervisor
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=tentacles
ExecStart=/usr/local/sbin/tentacles --config /etc/tentacles/config.yaml
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/tentacles /var/log/tentacles /run/tentacles
# Needs org.freedesktop.systemd1 access to start slot units — see below

[Install]
WantedBy=multi-user.target
```

D-Bus / systemd access: `tentacles` must be allowed to start/stop `tentacle@*.service` only. Use a narrow polkit rule or a systemd `user` transient scope. Do not give the daemon full root if you can avoid it. If v1 needs root for `systemd-run --uid=gha-runner`, document that as technical debt and confine with hardening.

`Type=notify`: the daemon sd_notifys `READY=1` only after:

- config validated
- payload verified/extracted
- GitHub client created
- scale set ensured
- listener session started

---

## 12. Environment and host toolchain

Jobs run as `gha-runner` on the host. That is the point of this design: reuse the Debian box’s compilers, `mise` tools, caches, and network.

Required work before declaring the daemon useful:

1. Create `gha-runner` with a home directory.
2. Install `mise` and the tools jobs need (Node, Go, etc.) as that user.
3. Put shims on `PATH` in `runner.env`.
4. Prove a trivial workflow can run `node -v` and `go version` under `systemd-run --uid=gha-runner --pty`.
5. Only then wire `run.sh`.

Cache policy:

- Shared caches (`~/.cache`, `mise`, `go/pkg/mod`) are allowed and desirable.
- They are also a cross-job residue channel. Accept that under the trusted-org constraint.
- `_work` is never shared.

Do not run jobs as a login shell. If a tool only works after `eval "$(mise activate bash)"`, fix `runner.env` instead.

---

## 13. Failure modes

| Failure | Detection | Action |
|---|---|---|
| Listener 401/403 | client error | Fail session, backoff, alert. Do not flap scale-set create. |
| Session drop | listener error | Reconnect. Existing runners stay up. |
| JIT generate fails | API error | Leave desired unsatisfied, retry next tick. Metric + log. |
| `run.sh` exits before `JobStarted` and before `acquire_grace` | process exit | Wipe slot, count as acquire failure. GitHub will reassign up to 3 times. |
| `run.sh` never starts | start timeout | Stop unit, wipe, backoff that slot ID. |
| Job canceled / reassigned | `JobCompleted` canceled | Do not scale on that message. Statistics drive desired. |
| Busy slot, daemon restart | units still running | On boot, scan `tentacle@*.service` and slot dirs; adopt or stop+wipe. |
| Disk full | extract/cleanup error | Stop starting new slots. Keep listener alive so `maxCapacity` can be lowered if you add that later. |
| Host reboot mid-job | units gone | GitHub times out / requeues. Cleanup leftover slot dirs on boot. |
| Runner payload SHA mismatch | verify | Refuse to start any slot. |
| `mise` / PATH broken | first job fails | Operational, not a daemon crash. Keep a canary workflow. |
| Upstream client API change | compile / test | Adapter absorbs it. Pin until tests pass. |

Boot adoption algorithm:

1. List systemd units `tentacle@*.service`.
2. List `slots/*`.
3. Running unit + matching dir → adopt as `busy` (conservative).
4. Dir without unit → wipe.
5. Unit without dir → stop unit.
6. Then start the listener and reconcile from statistics.

Never start a second `run.sh` in an occupied slot directory.

---

## 14. Observability

Logs: structured JSON to stderr → journald.

Required fields: `scale_set`, `slot`, `runner_name`, `desired`, `actual`, `event`, `err`.

Never: JIT payload, PEM, installation tokens.

Metrics on `127.0.0.1:9090` (plus a liveness `/healthz` on the same listener):

- `tentacles_desired_runners`
- `tentacles_actual_runners{state=}`
- `tentacles_jobs_started_total`
- `tentacles_jobs_completed_total{result=}`
- `tentacles_acquire_failures_total`
- `tentacles_slot_start_seconds` (histogram)
- `tentacles_listener_errors_total`
- `tentacles_last_message_id`

`_diag` copy is mandatory before production use. GitHub’s own autoscaling guidance says ephemeral runner logs must be forwarded externally or you cannot debug failed jobs.

---

## 15. Security

Hard rules:

- GitHub App only in v1.
- JIT is a secret. Mode `0600`, tmpfs, deleted after start.
- Slot units run as `gha-runner`, not root.
- No Docker socket mount, no passwordless sudo, no access to `/etc/tentacles/app.pem`.
- Daemon user cannot be written by `gha-runner`.
- `ProtectSystem=strict` on both units.
- Scale-set name is not a secret; the App key is.

Document in the README, prominently: this is not job isolation. Any workflow that can target `debian-host` can run as `gha-runner` on the machine. Restrict who can queue those workflows (org runner group + selected repos). That control is in GitHub, not in this daemon.

---

## 16. Implementation phases

Each phase has an exit criterion. Do not start the next phase without it.

### Phase 0 — Spike (1–2 days)

- Vendor / `go get github.com/actions/scaleset@v0.4.0`.
- Read `listener/listener.go`, `client.go`, `types.go`, `examples/dockerscaleset`.
- Confirm App credential fields and `GenerateJitRunnerConfig` arguments.
- Confirm how `run.sh` consumes JIT on the pinned runner version (file vs argv vs env).
- Confirm whether JIT can disable auto-update.
- Create a scratch GitHub App in a throwaway org/repo and list/create a scale set from a 50-line program.
- Decide systemd API: `dbus` vs `systemd-run` exec.

Exit: a short note in `docs/spike.md` with exact function names, the chosen JIT invocation, and any preview-API landmines.

### Phase 1 — Skeleton

- `cmd/tentacles`, config load, validation, `Type=notify`.
- Directory layout creation.
- Structured logging.
- Unit tests for config validation.

Exit: `tentacles --config config.example.yaml --dry-run` validates and exits 0.

### Phase 2 — Scale set session, no runners

- Adapter + listener.
- Metrics for desired count.
- Reconcile log line only: `desired=N actual=0`.
- Graceful SIGTERM: close session, do not delete the scale set.

Exit: queue a workflow with `runs-on: debian-host` and watch `desired` move from 0 to 1 to 0 without any local runner. Job stays queued. That is success.

### Phase 3 — One process slot

- Payload download + sha256 + extract.
- Copy template → `slots/0001`.
- Process backend (not systemd yet) starts `run.sh` with JIT as `gha-runner`.
- `runner.env` wired.
- Cleanup on exit.

Exit: the queued job from phase 2 actually runs `echo hello` and the slot directory is gone afterwards.

### Phase 4 — Reconcile N slots

- Slot manager with `max_runners`.
- Clamp logic.
- Busy marking from `JobStarted`.
- No kill of busy slots.
- Sequential scale-up, idle scale-down.
- Boot adoption.

Exit: two concurrent jobs on `debian-host` run on two slots; a third stays queued if `max_runners: 2`. After completion, `min_runners: 0` leaves zero processes.

### Phase 5 — systemd backend

- Replace process backend with transient or template units.
- Quotas from config.
- journald collection per unit.
- polkit / privilege story documented.
- `_diag` shipping.

Exit: `systemctl status tentacle@0001` during a job; `journalctl -u tentacle@0001` has runner output; unit disappears after cleanup.

### Phase 6 — Production hardening

- Disk watermark: refuse new slots under 10% free on the state filesystem.
- Payload refresh without downtime (new template dir, next slot uses it).
- Canary workflow in the target org.
- README: App permissions, runner group lock-down, `mise` setup, non-isolation warning.
- Pin GitHub Actions in *this* repo to build and test the daemon.

Exit: daemon survives 24h of start/stop, listener reconnect, and a forced `kill -9` of one `run.sh` without leaking slots or double-registering.

### Phase 7 — Optional

- `.deb` with systemd units and `gha-runner` user.
- Reflink copies.
- Parallel slot starts.
- Multiple scale sets on one host (different labels / runner groups).
- Nix unit, if you want it on NixOS later. Keep Debian first.

---

## 17. Testing

### Unit

- Config validation table tests.
- `clamp(desired, min, max)`.
- Reconcile decisions: start N, stop only idle, never stop busy.
- Slot ID allocation and reuse after wipe.
- Cleanup deletes JIT and `_work` even if `_diag` copy fails.

### Fake runner

`testdata/fake-runner/run.sh`:

- Reads JIT file/env and exits 2 if missing.
- Sleeps until `FAKE_RUNNER_EXIT` or signal.
- Writes a marker file so tests can see “claimed”.

Use this for reconcile tests without GitHub.

### Integration (manual / nightly)

Requires a real App and a disposable repo:

1. `min_runners: 0`, queue one job, assert one unit, assert cleanup.
2. Two jobs, `max_runners: 1`, assert serialization.
3. Kill `run.sh`, assert GitHub reassigns and daemon starts a replacement.
4. Restart `tentacles` mid-job, assert job is not SIGKILLed.
5. `node -v` via `mise` shims inside a real workflow.

Do not mock the upstream scale-set HTTP in the first integration test. The preview client is the risk; talk to the real API in a sandbox org.

---

## 18. Debian host bootstrap

One-time, not in the Go code:

1. Debian stable/testing, systemd, `curl`, `ca-certificates`, `jq`.
2. Users `tentacles` and `gha-runner`.
3. Groups and directory ownership as in section 4.
4. Official `installdependencies.sh` for the pinned runner version.
5. `mise` + toolchains as `gha-runner`.
6. GitHub App installed on the org; installation ID recorded.
7. Runner group: selected repositories only, label/scale-set `debian-host`.
8. Firewall: no new inbound; confirm egress to GitHub.
9. Install `tentacles.service`, enable, start.
10. Confirm scale set appears under org → Settings → Actions → Runners.

Workflow roll-out:

- Keep any existing standing runner until phase 4 is proven.
- Move one repo to `runs-on: debian-host`.
- Then retire the old `svc.sh` unit. Do not run both against the same work directory.

---

## 19. Acceptance criteria

The project is done for v1 when all of the following are true:

1. A Debian host with only `tentacles.service` persistent can execute org workflows that specify `runs-on: debian-host`.
2. Authentication is a GitHub App. No PAT in config or docs for the happy path.
3. No Docker daemon, no microVM, no Kubernetes is required at runtime.
4. No `config.sh` / `svc.sh` is used.
5. Idle with `min_runners: 0` means zero `run.sh` processes and zero slot directories.
6. Concurrent jobs up to `max_runners` each get their own slot directory and unit.
7. After every job, `_work`, JIT, and slot tree are gone; `_diag` is in `log_dir`.
8. `mise`-installed `node` is visible to jobs via `runner.env`.
9. Listener restart and daemon restart do not leak extra scale sets or extra runners.
10. README states the isolation limitation and the GitHub runner-group control.

---

## 20. Suggested first commit series

1. `chore: module skeleton and config types`
2. `feat: validate config and create state dirs`
3. `feat: scaleset adapter and listener loop`
4. `feat: runner payload fetch and verify`
5. `feat: process backend and single-slot JIT start`
6. `feat: reconciler with min/max clamp`
7. `feat: systemd backend and diag shipping`
8. `docs: debian bootstrap and threat model`

Keep the webhook prototype out of this tree. The scale-set listener replaces it. If any webhook code is reused, reuse only logging and env helpers, not the event model.
