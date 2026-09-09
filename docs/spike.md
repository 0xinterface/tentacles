# Phase 0 spike — actions/scaleset v0.4.0

Findings from reading `github.com/actions/scaleset@v0.4.0` source
(`client.go`, `types.go`, `listener/listener.go`, `session_client.go`,
`examples/dockerscaleset`). Exact names, verified against the module cache.

## Client construction (GitHub App)

```go
c, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
    GitHubConfigURL: "https://github.com", // org or repo URL works
    GitHubAppAuth: scaleset.GitHubAppAuth{
        ClientID:       "Iv1.xxx",  // string; App ID also works
        InstallationID: 12345678,   // int64, must be non-zero
        PrivateKey:     pemContent, // PEM *content*, NOT a path — daemon must os.ReadFile the configured path
    },
    SystemInfo: scaleset.SystemInfo{System: "tentacles", Version: ..., ScaleSetID: id},
})
```

Installation tokens are refreshed internally by the client; there is no
token plumbing on our side.

## Scale set ensure

- Default runner group has hardcoded ID `1` (`scaleset.DefaultRunnerGroup`).
  Non-default groups: `GetRunnerGroupByName(ctx, name) (*RunnerGroup, error)`.
- `GetRunnerScaleSet(ctx, runnerGroupID, name)` — list-then-reuse.
- `CreateRunnerScaleSet(ctx, &RunnerScaleSet{Name, RunnerGroupID, Labels, RunnerSetting})`.
- Labels default to the scale-set name; `Label{Type: "system", Name: ...}`
  (`applyDefaultLabelTypes` fills `Type` when empty).
- **Auto-update can be disabled**: `RunnerSetting{DisableUpdate: true}` at
  scale-set creation. Confirmed by `examples/dockerscaleset/main.go:82`.
  So: set it, and also pin the tarball version in config.

## Listener / message loop

The upstream `listener` package already implements the whole loop the plan
describes — use it, do not hand-roll `GetMessage`:

```go
sessionClient, err := c.MessageSessionClient(ctx, scaleSetID, owner) // owner = hostname
l, err := listener.New(sessionClient, listener.Config{ScaleSetID: id, MaxRunners: n, Logger: lg})
err := l.Run(ctx, scaler) // blocks; returns error on failure → restart with backoff
```

- `MessageSessionClient` implements `listener.Client`
  (`GetMessage`, `DeleteMessage`, `AcquireJobs`, `Session`).
- Session create/refresh/delete, `AcquireJobs`, and `DeleteMessage` acking
  are all handled inside the listener. Process-then-ack ordering is upstream's.
- `l.SetMaxRunners(n)` is concurrency-safe during `Run`.
- `GetMessage` long-polls ~50s; nil message on 202 → loop continues (upstream).
- On `Run` start it fires `HandleDesiredRunnerCount(ctx, session.Statistics.TotalAssignedJobs)`
  from the initial session — desired count is always statistics-driven.

`listener.Scaler` (what we implement):

```go
type Scaler interface {
    HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error
    HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error
    HandleDesiredRunnerCount(ctx context.Context, count int) (int, error)
}
```

`RunnerScaleSetStatistic.TotalAssignedJobs` is the only scaling signal.
`JobStarted.RunnerName`, `JobCompleted{RunnerName, Result}` for busy-marking
and metrics.

`MessageSessionClient.Close(ctx)` deletes the session — call on graceful
shutdown. **Never** `DeleteRunnerScaleSet` on daemon restart/shutdown
(the upstream example deletes; we must not).

## JIT config

```go
cfg, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
    Name:       runnerName,  // e.g. debian-host-0001-x7f3
    WorkFolder: workFolder,  // the slot's _work
}, scaleSetID)
// cfg.EncodedJITConfig — secret; cfg.Runner.ID/Name
```

## How run.sh consumes JIT (pinned runner 2.328.x)

The official agent only supports `./run.sh --jitconfig <encoded-value>` —
value as argv. There is no `--jitconfig-file` flag and no env-var input on
the official runner (ARC's `RUNNER_JIT_CONFIG` env var is entrypoint.sh
plumbing that still ends up as argv).

**Decision (plan §11 fallback 3, accepted):** launch through a shell so the
value never sits in a long-lived argv:

```sh
exec ./run.sh --jitconfig "$(cat /run/tentacles/0001.jit)"
```

JIT file is `0600` on tmpfs, written by the daemon, read by the shell at
start, unlinked when the slot is wiped. Residual `ps` exposure window is
milliseconds on a trusted host.

## systemd API choice

**Decision: `systemd-run` transient units**, not dbus and not a static
template. Rationale: every knob (User, CPUQuota, MemoryMax, EnvironmentFile,
hardening props) then comes from config as `-p` properties — no root-owned
drop-in files to render, no dbus/cgo dependency, compiles and unit-tests
anywhere (tests inject fake `systemd-run`/`systemctl` via `PATH`).

Unit naming: `tentacle-<id>.service` (e.g. `tentacle-0001.service`) —
`systemd-run --unit=tentacle-0001`. Transient units unload when the process
exits; boot adoption lists `tentacle-*.service`.

## Preview-API landmines

- `listener.Config.Validate()` requires `MaxRunners >= 0`; `ScaleSetID != 0`.
- `slog.DiscardHandler` in listener defaults requires Go ≥ 1.24 — we require 1.25.
- `CreateRunnerScaleSet` 409s on duplicate names — always Get-then-Create.
- Everything above is behind `internal/scaleset`; upstream types must not
  leak past that package.
