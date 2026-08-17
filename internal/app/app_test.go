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
	mu       sync.Mutex
	events   scaleset.Events
	ensured  bool
	jitCount int
	runFn    func(ctx context.Context, f *fakeScaleSet) error
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

func (f *fakeScaleSet) GenerateJIT(_ context.Context, runnerName, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jitCount++
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
func writeIntegrationConfig(t *testing.T, dir, tarURL, sha string, minRunners, maxRunners int) string {
	t.Helper()
	envFile := filepath.Join(dir, "runner.env")
	cwd, _ := os.Getwd()
	if err := os.WriteFile(envFile, []byte(fmt.Sprintf("PATH=%s:/bin:/usr/bin\nHOME=%s\n", filepath.Join(cwd, "bin-tmp"), dir)), 0o644); err != nil {
		t.Fatal(err)
	}
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
		filepath.Join(dir, "jit"), freePort(t))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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
	cfgPath := writeIntegrationConfig(t, dir, srv.URL, sha, 0, 2)

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
	cfgPath := writeIntegrationConfig(t, dir, srv.URL, sha, 1, 2)

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
	cfgPath := writeIntegrationConfig(t, dir, srv.URL, sha, 0, 2)

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
