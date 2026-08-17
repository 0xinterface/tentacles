# gh-runnerd

Single-host GitHub Actions runner supervisor for Debian. The daemon
authenticates as a **GitHub App**, owns one runner scale set via
[`github.com/actions/scaleset`](https://github.com/actions/scaleset), and
starts official [`actions/runner`](https://github.com/actions/runner)
processes with **just-in-time (JIT) configuration**. Each process is an
ephemeral, one-job slot: after the job exits, the slot directory, `_work`,
and JIT material are wiped and the next job gets a fresh copy of the
payload.

No Docker, no microVMs, no Kubernetes, no `config.sh`, no `svc.sh`, no
inbound webhooks.

## Security warning — read this first

> **This is not job isolation.** Jobs run as the `gha-runner` Unix user
> directly on the host, with host toolchains, caches, and network.
> **Anyone who can queue a workflow targeting the scale set can run code
> on this machine.** The only control that matters is in GitHub, not in
> this daemon: restrict *who* can queue those workflows via an **org
> runner group restricted to selected repositories**, and only install the
> App on orgs you trust. Treat this host as trusted-org workload only
> (plan §15).

## How it works

```
                    GitHub Actions service
                    (scale set + job assign)
                              ^
                              | App JWT / installation token
                              | session + long poll + JIT
                              v
                     gh-runnerd.service
                     (always on, no job code)
                              |
              +---------------+----------------+
              | slot alloc | payload | systemd |
              +---------------+----------------+
                              |
           gha-slot-0001     gha-slot-0002     gha-slot-000N
           run.sh --jitconfig
           User=gha-runner
```

- The scale-set **listener** (upstream `listener` package) long-polls the
  message API. `statistics.TotalAssignedJobs` is the *only* scaling signal.
- A reconciler keeps `min(max(desired, min_runners), max_runners)`
  official runner processes alive.
- Each slot is `systemd-run` transient unit `gha-slot-<id>.service`
  (or the static `gha-slot@.service` template, see below) running
  `./run.sh --jitconfig "$(cat /run/gh-runnerd/<id>.jit)"` as
  `gha-runner` — the JIT value is read from a `0600` tmpfs file by the
  shell so it never sits in a long-lived argv (see `docs/spike.md`).
- Workflows target the scale-set name, not `self-hosted`:

  ```yaml
  jobs:
    test:
      runs-on: debian-host
  ```

## Quickstart

### 1. GitHub App

1. Create a GitHub App (org or repo scope). Record the **client id** and
   **installation id**; save the private key PEM.
2. Grant the App the scale-set permission:
   - **Organization** scale set: `organization_self_hosted_runners`
   - **Repository** scale set: `administration`
3. Install the App on the org (or repo) that owns the scale set and
   record the installation id.
4. Restrict the runner group to **selected repositories only** (the
   security warning above is about exactly this).

### 2. Host bootstrap (one-time, as root)

```sh
sudo ./scripts/install-host-deps.sh        # libicu/krb5/zlib + curl/ca-certificates/jq/git
```

Then, per plan §18 and §4:

- Create users: `gh-runnerd` (daemon; must not be writable by
  `gha-runner`) and `gha-runner` (jobs; no sudo, no Docker socket).
- Create directories: `/etc/gh-runnerd`, `/var/lib/gh-runnerd`,
  `/var/cache/gh-runnerd`, `/var/log/gh-runnerd`, `/run/gh-runnerd`
  (tmpfs JIT files).
- Install `mise` and the toolchains jobs need **as `gha-runner`**, and
  put the shims on `PATH` in `/etc/gh-runnerd/runner.env` (see
  Configuration). Do **not** source `.bashrc` — see FAQ.
- Run the payload's own `bin/installdependencies.sh` once per
  host/version if you want its exact list (the script above covers the
  same base deps for the pinned 2.328 line).

### 3. Build and configure

```sh
go build ./cmd/gh-runnerd        # Go 1.25+; binary -> /usr/local/sbin/gh-runnerd
sudo install -m 0755 gh-runnerd /usr/local/sbin/gh-runnerd
sudo install -m 0600 app.pem /etc/gh-runnerd/app.pem
sudo cp configs/config.example.yaml /etc/gh-runnerd/config.yaml
# edit /etc/gh-runnerd/config.yaml: App creds, scope, capacity, runner
```

Validate without starting anything:

```sh
gh-runnerd --config /etc/gh-runnerd/config.yaml --dry-run   # exits 0 if valid
```

### 4. Enable the service

```sh
sudo install -m 0644 configs/systemd/gh-runnerd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runnerd
```

Confirm the scale set appears under org → Settings → Actions → Runners,
then move one repo to `runs-on: debian-host` while keeping any standing
runner until it is proven, and retire old `svc.sh` units — never run two
agents against the same work directory (plan §18).

## Configuration

Full reference: `configs/config.example.yaml`; validation rules in
plan §6.

| Section | Controls |
|---|---|
| `github` | API URL, App `client_id`/`installation_id`/`private_key_path`, scope (`organization` or `repository`, owner, repo) |
| `scale_set` | `name` (this is `runs-on`), `runner_group`, `extra_labels` |
| `capacity` | `min_runners`, `max_runners`, `job_cpu_quota_percent`, `job_memory_max` |
| `runner` | pinned `version`, `download_url`, `sha256`, `work_directory`, `disable_update`, `user`, `environment_file` |
| `paths` | `state_dir`, `cache_dir`, `log_dir` |
| `runtime` | `backend` (`systemd` \| `process`), slot start/stop/cleanup timeouts, `acquire_grace` |
| `observability` | metrics `listen` address, `log_level`, `ship_diag` |

Validation is fail-closed on boot: `max_runners >= 1`, `min_runners <=
max_runners`, `max_runners` under the hard cap (32); PEM readable and
`installation_id > 0`; `scale_set.name` a valid Actions label (no
spaces); `runner.sha256` set unless `GH_RUNNERD_ALLOW_UNVERIFIED_PAYLOAD=1`;
`environment_file` exists; `backend=systemd` only if `/run/systemd/system`
exists; state/cache dirs writable.

`runner.env` is a systemd `EnvironmentFile`, not YAML:

```dotenv
PATH=/home/gha-runner/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
HOME=/home/gha-runner
MISE_DATA_DIR=/home/gha-runner/.local/share/mise
LANG=C.UTF-8
```

The daemon refuses to start jobs until this file exists — this is the fix
for the "systemd does not see `mise`/`node`" failure mode (plan §12).

## Scaling semantics

- Desired = `statistics.TotalAssignedJobs`, then
  `desired = clamp(desired, min_runners, max_runners)`.
- The listener's job-lifecycle messages (`JobStarted`/`JobCompleted`)
  never drive scaling — they only mark slots busy and feed metrics.
- `min_runners: 1` keeps one idle `run.sh` registered as a warm pool
  (latency knob); `min_runners: 0` scales to **zero processes and zero
  slot directories** when idle.
- Scale-down stops only `idle`/`starting` slots (oldest first). **Busy
  slots are never killed**, no matter how far desired drops.
- A slot in `starting` counts toward `actual` so reconcile never
  overshoots `max_runners`; message bodies cap at 50 items and can
  truncate a backlog, but statistics already reflect current truth.

## Observability

- **Logs**: structured JSON via `log/slog` to stderr → journald
  (`journalctl -u gh-runnerd -o json`). Fields: `scale_set`, `slot`,
  `runner_name`, `desired`, `actual`, `event`, `err`. Never logged: JIT
  payload, PEM, installation tokens.
- **Metrics** on `127.0.0.1:9090` (hand-rolled Prometheus text
  exposition):

  - `gh_runnerd_desired_runners`
  - `gh_runnerd_actual_runners{state=}`
  - `gh_runnerd_jobs_started_total`
  - `gh_runnerd_jobs_completed_total{result=}`
  - `gh_runnerd_acquire_failures_total`
  - `gh_runnerd_slot_start_seconds` (histogram)
  - `gh_runnerd_listener_errors_total`
  - `gh_runnerd_last_message_id`

- **`_diag` shipping**: when `observability.ship_diag` is on, each slot's
  `_diag` directory is copied to
  `/var/log/gh-runnerd/<slot>-<utc>-<runnerName>/` before the slot is
  wiped — required before production use, or failed ephemeral jobs are
  undebuggable.

## systemd integration

- `gh-runnerd.service` is `Type=notify`: it sd_notifies `READY=1` only
  after config is validated, payload verified/extracted, GitHub client
  created, scale set ensured, and the listener session started (plan §11).
- Slot units are **transient** by default: the daemon runs
  `systemd-run --unit=gha-slot-<id>` with `-p` properties taken from
  config (User, CPUQuota, MemoryMax, EnvironmentFile, hardening) — no
  root-owned drop-ins, and tests inject fake binaries via `PATH`.
  `configs/systemd/gha-slot@.service` is a **static alternative** with
  equivalent properties for operators who prefer fixed units.
- Unprivileged slot control: `gh-runnerd` needs a narrow polkit rule for
  `org.freedesktop.systemd1` limited to `gha-slot-*.service`, or a systemd
  user transient scope. If polkit is too heavy for v1, the daemon may run
  as root with the unit hardening (`NoNewPrivileges`, `ProtectSystem=strict`)
  — documented technical debt.
- **Boot adoption** (plan §13): on start the daemon lists
  `gha-slot-*.service` units and `slots/*` dirs — running unit + matching
  dir is adopted as `busy`, dir without unit is wiped, unit without dir is
  stopped; only then does it start the listener and reconcile. A daemon
  restart never SIGKILLs a running job, and never leaks extra scale sets
  or runners.

## Development

```
go test ./...        # all packages; no cgo, runs on darwin
```

- `internal/scaleset` is the only package importing
  `github.com/actions/scaleset`; upstream types never leak past it
  (upgrade risk is contained to one adapter).
- Reconcile/slot tests use the process backend plus
  `testdata/fake-runner/run.sh` (a stub agent: fails without JIT, sleeps
  until `FAKE_RUNNER_EXIT`, writes a "claimed" marker).
- The systemd backend execs `systemd-run`/`systemctl`; tests inject fake
  binaries via `PATH`. Linux-only behavior goes through exec'd binaries
  or `runtime.GOOS` checks.
- The scale-set client is a **public preview**: pinned at
  `github.com/actions/scaleset v0.4.0` — read the upstream changelog
  before bumping.

### Phase status (plan §16)

| Phase | Status |
|---|---|
| 0 — spike (JIT invocation, systemd API, landmines) | done — `docs/spike.md` |
| 1 — skeleton (config, `--dry-run`, notify) | done |
| 2 — scale-set session + listener | code complete; offline tests green |
| 3 — one process slot | code complete; e2e test with fake runner green |
| 4 — reconcile N slots | code complete; unit + e2e tests green |
| 5 — systemd backend | code complete; fake-binary tests green; **needs on-host verification** |
| 6 — production hardening | disk watermark + this README + pinned CI done; 24h soak, canary workflow, live GitHub verification pending |
| 7 — optional (.deb, reflink, parallel starts) | not started |

Phases 2–5 are verified by offline tests (`go test ./...`, including an
end-to-end daemon test with a fake scale set, process backend, and the
fake runner). Live verification against a real GitHub App and a Debian
host (plan §17 integration checklist) has not been performed yet.

## Known limitations & FAQ

- **No isolation.** See the warning at the top. Control who can queue
  jobs via the GitHub runner group; the daemon cannot.
- **Single host, single scale set, single App installation.** Not a
  multi-host scheduler; one installation, one scale set, one host.
- **Preview client pin.** `actions/scaleset` is a preview API. The module
  is pinned; interface drift is absorbed by `internal/scaleset`.
- **Job env comes from `EnvironmentFile`, not `.bashrc`.** Jobs are not
  login shells; if a tool only works after `eval "$(mise activate bash)"`,
  fix `/etc/gh-runnerd/runner.env` instead (this is the mise PATH failure
  mode from plan §12).
- **JIT argv exposure.** `run.sh` only accepts `--jitconfig <value>` on
  the pinned 2.328 line, so the value passes through `ps` for
  milliseconds (read from a `0600` tmpfs file by the shell). Acceptable
  on a trusted host; revisit if a `--jitconfig-file` flag ever lands.
- **One job per process.** After every exit the slot's `_work`, JIT files,
  and the entire slot tree are wiped and re-copied from the template.
- **Host toolchains.** `mise`, `~/.cache`, and `go/pkg/mod` are shared
  across jobs by design — that is also a cross-job residue channel;
  accepted under the trusted-org constraint.

## Acceptance criteria (plan §19, mapped)

1. Debian host with only `gh-runnerd.service` persistent runs `runs-on: debian-host` workflows — Quickstart + systemd integration.
2. GitHub App auth, no PAT — GitHub App section; config validation requires PEM + installation id.
3. No Docker/microVM/K8s — design above; slot units are `exec` + systemd.
4. No `config.sh`/`svc.sh` — JIT replaces registration; only `gh-runnerd.service` is persistent.
5. Idle `min_runners: 0` → zero processes and zero slot dirs — Scaling semantics + ephemeral cleanup.
6. Concurrent jobs ≤ `max_runners` each get their own slot dir and unit — Scaling semantics.
7. After every job `_work`, JIT, slot tree gone; `_diag` in `log_dir` — Observability (`_diag` shipping).
8. `mise`-installed `node` visible to jobs via `runner.env` — Configuration (`EnvironmentFile`).
9. Listener/daemon restarts leak no scale sets or runners — systemd integration (boot adoption); spike ("never delete scale set").
10. README states the isolation limitation and runner-group control — Security warning above.

Implementation plan: `gh-runnerd-implementation-plan.md`. Spike notes:
`docs/spike.md`.
