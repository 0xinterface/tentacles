package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkust/gh-runnerd/internal/scaleset"
)

// fakeScaleSet drives the daemon through the same surface as the real
// adapter: it reports a scale set, pushes desired counts, and mints
// deterministic fake JIT configs.
type fakeScaleSet struct {
	mu          sync.Mutex
	events      scaleset.Events
	ensured     bool
	jitCount    int
	workFolders []string
	runFn       func(ctx context.Context, f *fakeScaleSet) error
}

func (f *fakeScaleSet) SetEvents(ev scaleset.Events) {
	f.mu.Lock()
	f.events = ev
	f.mu.Unlock()
}

func (f *fakeScaleSet) EnsureScaleSet(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = true
	return nil
}

func (f *fakeScaleSet) ScaleSetID() int { return 42 }

func (f *fakeScaleSet) GenerateJIT(_ context.Context, runnerName, workFolder string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jitCount++
	f.workFolders = append(f.workFolders, workFolder)
	return "fake-jit-" + runnerName, nil
}

func (f *fakeScaleSet) Run(ctx context.Context) error {
	f.mu.Lock()
	runFn := f.runFn
	f.mu.Unlock()
	if runFn == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return runFn(ctx, f)
}

func (f *fakeScaleSet) pushDesired(n int) {
	f.mu.Lock()
	ev := f.events
	f.mu.Unlock()
	if ev.Desired != nil {
		ev.Desired(n)
	}
}

func (f *fakeScaleSet) jobStart(name string) {
	f.mu.Lock()
	ev := f.events
	f.mu.Unlock()
	if ev.JobStart != nil {
		ev.JobStart(name)
	}
}

func (f *fakeScaleSet) pushMessageID(id int64) {
	f.mu.Lock()
	ev := f.events
	f.mu.Unlock()
	if ev.MessageID != nil {
		ev.MessageID(id)
	}
}

func (f *fakeScaleSet) sessionStart() {
	f.mu.Lock()
	ev := f.events
	f.mu.Unlock()
	if ev.SessionStarted != nil {
		ev.SessionStarted()
	}
}

func (f *fakeScaleSet) startedJITs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jitCount
}

// fixtureRunnerTar builds a runner tarball whose run.sh delegates to
// testdata/fake-runner/run.sh, creating a _diag entry first so diag
// shipping is exercised end to end.
func fixtureRunnerTar(t *testing.T) ([]byte, string) {
	t.Helper()
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-runner", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSh := fmt.Sprintf("#!/bin/sh\nmkdir -p _diag\necho diag-line > _diag/runner.log\nexec '%s' \"$@\"\n", fake)

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	write := func(name string, mode int64, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("run.sh", 0o755, runSh)
	write("bin/runhelper", 0o755, "#!/bin/sh\nexit 0\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(gzBuf.Bytes())
	return gzBuf.Bytes(), hex.EncodeToString(sum[:])
}

func genPEM(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.pem")
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return fmt.Sprintf("127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
}

// writeIntegrationConfig assembles a fully valid config for the process
// backend, with the runner tarball served from srv.
func writeIntegrationConfig(t *testing.T, dir, tarURL, sha string, minRunners, maxRunners int, extraEnv ...string) (string, string) {
	t.Helper()
	envFile := filepath.Join(dir, "runner.env")
	cwd, _ := os.Getwd()
	envBody := fmt.Sprintf("PATH=%s:/bin:/usr/bin\nHOME=%s\n", filepath.Join(cwd, "bin-tmp"), dir)
	for _, kv := range extraEnv {
		envBody += kv + "\n"
	}
	if err := os.WriteFile(envFile, []byte(envBody), 0o644); err != nil {
		t.Fatal(err)
	}
	listenAddr := freePort(t)
	stateDir := filepath.Join(dir, "state")
	cfg := fmt.Sprintf(`github:
  url: https://github.com
  app:
    client_id: Iv1.testing
    installation_id: 12345678
    private_key_path: %s
  scope:
    kind: organization
    owner: test-org

scale_set:
  name: test-host
  runner_group: default

capacity:
  min_runners: %d
  max_runners: %d
  job_cpu_quota_percent: 100
  job_memory_max: 1G

runner:
  version: 2.328.0
  download_url: %s
  sha256: %s
  work_directory: _work
  disable_update: true
  user: %s
  environment_file: %s

paths:
  state_dir: %s
  cache_dir: %s
  log_dir: %s

runtime:
  backend: process
  slot_start_timeout: 30s
  slot_stop_timeout: 15s
  cleanup_timeout: 15s
  acquire_grace: 120s
  jit_dir: %s

observability:
  listen: %s
  log_level: warn
  ship_diag: true
`,
		genPEM(t, dir), minRunners, maxRunners, tarURL, sha,
		currentUser(t), envFile,
		stateDir, filepath.Join(dir, "cache"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "jit"), listenAddr)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, listenAddr
}
func currentUser(t *testing.T) string {
	t.Helper()
	u := os.Getenv("USER")
	if u == "" {
		u = "nobody"
	}
	return u
}

func poll(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// TestDaemonScaleUpDownEphemeral is the plan's phase 3+4 exit criterion
// in miniature, fully offline: a queued "job" (desired=1) materializes a
// slot from a verified payload, starts the fake runner with a JIT, ships
// its _diag, and — once desired drops to 0 — wipes the slot entirely.
func TestDaemonScaleUpDownEphemeral(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	tarBytes, sha := fixtureRunnerTar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath, listenAddr := writeIntegrationConfig(t, dir, srv.URL, sha, 0, 2)

	fake := &fakeScaleSet{}
	fake.runFn = func(ctx context.Context, f *fakeScaleSet) error {
		f.pushDesired(1)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{ConfigPath: cfgPath, ScaleSet: fake})
	}()

	stateDir := filepath.Join(dir, "state")
	slotsDir := filepath.Join(stateDir, "slots")
	slotDir := filepath.Join(slotsDir, "0001")

	// Scale up: slot materialized, runner started with a non-empty JIT.
	if !poll(t, 30*time.Second, func() bool {
		m := filepath.Join(slotDir, ".fake-claimed")
		b, err := os.ReadFile(m)
		return err == nil && len(strings.TrimSpace(string(b))) > 0
	}) {
		cancel()
		t.Fatalf("runner never claimed; slots dir: %v", ls(t, slotsDir))
	}
	t.Logf("slot 0001 running")

	// JIT file exists with 0600 while the slot is live.
	jitPath := filepath.Join(dir, "jit", "0001.jit")
	if fi, err := os.Stat(jitPath); err != nil {
		t.Errorf("jit file missing while slot live: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("jit file mode = %v, want 0600", fi.Mode().Perm())
	}

	// The slot_start histogram observes exactly one start per slot
	// (materialize and JIT mint used to double-count it).
	if out, err := http.Get("http://" + listenAddr + "/metrics"); err != nil {
		t.Errorf("metrics scrape failed: %v", err)
	} else {
		b, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if want := "gh_runnerd_slot_start_seconds_count 1"; !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q (double-counted start?)", want)
		}
	}

	// Scale down: slot dir, jit file, and processes are gone.
	fake.pushDesired(0)
	if !poll(t, 30*time.Second, func() bool { return !dirExists(slotDir) }) {
		cancel()
		t.Fatalf("slot dir not wiped after scale-down: %v", ls(t, slotsDir))
	}
	if !poll(t, 5*time.Second, func() bool { return !fileExists(jitPath) }) {
		t.Errorf("jit file not unlinked after wipe")
	}

	// Diagnostics were shipped before the wipe.
	if !poll(t, 5*time.Second, func() bool {
		for _, e := range ls(t, filepath.Join(dir, "logs")) {
			if strings.Contains(e, "0001-") {
				return true
			}
		}
		return false
	}) {
		t.Errorf("no diag dir shipped: %v", ls(t, filepath.Join(dir, "logs")))
	}

	// Graceful shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon did not shut down")
	}

	// Scale to zero means zero slot directories (plan §19.5).
	if entries := ls(t, slotsDir); len(entries) != 0 {
		t.Errorf("slot dirs leaked after shutdown: %v", entries)
	}
}

// TestDaemonMinRunnersWarmPool: min_runners=1 keeps one idle runner even
// while statistics say zero assigned jobs.
func TestDaemonMinRunnersWarmPool(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	tarBytes, sha := fixtureRunnerTar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath, _ := writeIntegrationConfig(t, dir, srv.URL, sha, 1, 2)

	fake := &fakeScaleSet{}
	fake.runFn = func(ctx context.Context, f *fakeScaleSet) error {
		f.pushDesired(0) // statistics: idle
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{ConfigPath: cfgPath, ScaleSet: fake})
	}()

	slotDir := filepath.Join(dir, "state", "slots", "0001")
	if !poll(t, 30*time.Second, func() bool { return fileExists(filepath.Join(slotDir, ".fake-claimed")) }) {
		cancel()
		t.Fatalf("warm-pool runner never started")
	}
	// Desired 0 must NOT kill the min pool runner.
	time.Sleep(2 * time.Second)
	if !dirExists(slotDir) {
		t.Errorf("min_runners runner was scaled down below the floor")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon did not shut down")
	}
	// Shutdown stops idle slots.
	if !poll(t, 30*time.Second, func() bool { return !dirExists(slotDir) }) {
		t.Errorf("idle slot survived shutdown: %v", ls(t, filepath.Join(dir, "state", "slots")))
	}
}

// TestDryRunValidatesConfig is the plan's phase 1 exit criterion.
func TestDryRunValidatesConfig(t *testing.T) {
	tarBytes, sha := fixtureRunnerTar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfgPath, _ := writeIntegrationConfig(t, dir, srv.URL, sha, 0, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, Options{ConfigPath: cfgPath, DryRun: true}); err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestDaemonReplenishesWarmPoolAfterExit: plan §9 — "a local watcher
// pushes process exits" into reconcile. A warm-pool runner that dies is
// replaced without waiting for the next statistics push (the periodic
// tick from plan §13 is the backstop; the exit nudge is the fast path).
func TestDaemonReplenishesWarmPoolAfterExit(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	tarBytes, sha := fixtureRunnerTar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath, _ := writeIntegrationConfig(t, dir, srv.URL, sha, 1, 2, "FAKE_RUNNER_SLEEP=1")

	fake := &fakeScaleSet{}
	fake.runFn = func(ctx context.Context, f *fakeScaleSet) error {
		f.pushDesired(0) // statistics idle; the min pool keeps desired=1
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{ConfigPath: cfgPath, ScaleSet: fake}) }()

	// Each fake runner exits after ~1s; the daemon must keep minting
	// replacements for the warm slot.
	if !poll(t, 30*time.Second, func() bool { return fake.startedJITs() >= 3 }) {
		cancel()
		t.Fatalf("warm pool not replenished after runner exits; jit mints = %d", fake.startedJITs())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon did not shut down")
	}
}

// TestDaemonReadinessGatedOnSessionStart: plan §11 — sd_notify READY=1
// only after the listener session started, never when the daemon shuts
// down before a session came up. Also proves the last_message_id metric
// is wired end to end (plan §14).
func TestDaemonReadinessGatedOnSessionStart(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	tarBytes, sha := fixtureRunnerTar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	runCase := func(t *testing.T, fireSession bool) {
		dir := t.TempDir()
		cfgPath, listenAddr := writeIntegrationConfig(t, dir, srv.URL, sha, 0, 2)
		fake := &fakeScaleSet{}
		fake.runFn = func(ctx context.Context, f *fakeScaleSet) error {
			if fireSession {
				f.sessionStart()
				f.pushMessageID(7)
			}
			<-ctx.Done()
			return ctx.Err()
		}

		var mu sync.Mutex
		var states []string
		orig := sdNotify
		sdNotify = func(state string) error {
			mu.Lock()
			states = append(states, state)
			mu.Unlock()
			return nil
		}
		defer func() { sdNotify = orig }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- Run(ctx, Options{ConfigPath: cfgPath, ScaleSet: fake}) }()

		if fireSession {
			if !poll(t, 15*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(states) > 0
			}) {
				t.Fatal("READY=1 never sent after session start")
			}
			mu.Lock()
			first := states[0]
			mu.Unlock()
			if first != "READY=1" {
				t.Fatalf("first sd_notify = %q, want READY=1", first)
			}
			// The last message ID reached the metrics registry.
			if !poll(t, 5*time.Second, func() bool {
				out, err := http.Get("http://" + listenAddr + "/metrics")
				if err != nil {
					return false
				}
				defer out.Body.Close()
				b, _ := io.ReadAll(out.Body)
				return strings.Contains(string(b), "gh_runnerd_last_message_id 7")
			}) {
				t.Error("gh_runnerd_last_message_id not exposed after MessageID event")
			}
		} else {
			time.Sleep(2 * time.Second)
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run returned error: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("daemon did not shut down")
			}
			time.Sleep(200 * time.Millisecond) // let the notify goroutine settle
			mu.Lock()
			defer mu.Unlock()
			if len(states) != 0 {
				t.Fatalf("sd_notify fired without a listener session: %v", states)
			}
		}
	}
	t.Run("session started", func(t *testing.T) { runCase(t, true) })
	t.Run("no session before shutdown", func(t *testing.T) { runCase(t, false) })
}
