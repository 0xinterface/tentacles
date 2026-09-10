package payload

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cachedPayload(t *testing.T) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	if err := os.MkdirAll(env.cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath(env), fixture, 0644); err != nil {
		t.Fatal(err)
	}
	return env, sha256Hex(fixture)
}

func TestEnsureCanceledCachedPayloadDoesNotMaterialize(t *testing.T) {
	env, sha := cachedPayload(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := env.m.Ensure(ctx, fixtureVersion, sha, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure=%v, want cancellation", err)
	}
	if _, err := os.Stat(env.templateDir); !os.IsNotExist(err) {
		t.Fatal("canceled Ensure materialized template")
	}
	if _, err := os.Stat(env.templateDir + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("canceled Ensure left staging directory")
	}
}

// Cancellation at the existing extraction event deterministically exercises
// the final boundary before replacing a usable installed template.
type cancelAtEvent struct {
	slog.Handler
	event  string
	cancel context.CancelFunc
}

func (h cancelAtEvent) Enabled(context.Context, slog.Level) bool { return true }
func (h cancelAtEvent) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.event {
		h.cancel()
	}
	return nil
}

func TestEnsureCancellationPreservesInstalledTemplate(t *testing.T) {
	for _, event := range []string{"payload checksum verified", "payload extracted"} {
		t.Run(event, func(t *testing.T) {
			env, sha := cachedPayload(t)
			if err := os.Mkdir(env.templateDir, 0755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(env.templateDir, "installed")
			if err := os.WriteFile(sentinel, []byte("old template"), 0644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env.m.log = slog.New(cancelAtEvent{Handler: slog.DiscardHandler, event: event, cancel: cancel})
			if err := env.m.Ensure(ctx, fixtureVersion, sha, ""); !errors.Is(err, context.Canceled) {
				t.Fatalf("Ensure=%v, want cancellation", err)
			}
			got, err := os.ReadFile(sentinel)
			if err != nil || string(got) != "old template" {
				t.Fatalf("installed template changed: %q, %v", got, err)
			}
			if _, err := os.Stat(env.templateDir + ".tmp"); !os.IsNotExist(err) {
				t.Fatal("canceled extraction left staging directory")
			}
		})
	}
}

type cancelAfterRead struct {
	reader    io.Reader
	cancel    context.CancelFunc
	remaining int
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.remaining -= n
	if r.remaining <= 0 {
		r.cancel()
	}
	return n, err
}

func TestContextReaderReportsCancellationOnFinalRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelAfterRead{reader: bytes.NewReader([]byte("payload")), cancel: cancel, remaining: 1}
	// bytes.Reader returns its final bytes before EOF; a producer may return
	// both together. Ensure cancellation is not hidden by that legal EOF.
	reader := contextReader{ctx: ctx, r: readerFunc(func(p []byte) (int, error) { n, _ := r.Read(p); return n, io.EOF })}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy=%v, want cancellation", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestHashStopsWhenInputCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := bytes.NewReader(bytes.Repeat([]byte("x"), 2<<20))
	reader := &cancelAfterRead{reader: input, cancel: cancel, remaining: 64 << 10}
	if _, err := hashContext(ctx, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash=%v, want cancellation", err)
	}
	if input.Len() == 0 {
		t.Fatal("hash consumed remaining input after cancellation")
	}
}

func TestExtractEntryStopsWhenInputCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	data := bytes.Repeat([]byte("x"), 2<<20)
	if err := tw.WriteHeader(&tar.Header{Name: "large", Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	reader := &cancelAfterRead{reader: bytes.NewReader(archive.Bytes()), cancel: cancel, remaining: 512 + (64 << 10)}
	tr := tar.NewReader(reader)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := New(t.TempDir(), t.TempDir(), nil)
	if err := m.extractEntry(ctx, tr, hdr, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("extract=%v, want cancellation", err)
	}
	info, err := os.Stat(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 || info.Size() >= int64(len(data)) {
		t.Fatalf("extracted size=%d; expected stopped partial write", info.Size())
	}
}

func TestCopyFileCanceledDoesNotTruncateDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	for _, path := range []string{src, dst} {
		if err := os.WriteFile(path, []byte("preserve"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFileModeContext(ctx, src, dst, 0644); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy=%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "preserve" {
		t.Fatal("canceled copy truncated destination")
	}
}

type pauseAtVerification struct {
	slog.Handler
	entered chan struct{}
	release chan struct{}
}

func (h pauseAtVerification) Enabled(context.Context, slog.Level) bool { return true }
func (h pauseAtVerification) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "payload checksum verified" {
		close(h.entered)
		<-h.release
	}
	return nil
}

func TestEnsureWaitingForAnotherEnsureRespectsCancellation(t *testing.T) {
	env, sha := cachedPayload(t)
	entered, release := make(chan struct{}), make(chan struct{})
	env.m.log = slog.New(pauseAtVerification{Handler: slog.DiscardHandler, entered: entered, release: release})
	first := make(chan error, 1)
	go func() { first <- env.m.Ensure(context.Background(), fixtureVersion, sha, "") }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- env.m.Ensure(ctx, fixtureVersion, sha, "") }()
	completed := false
	select {
	case err := <-second:
		completed = true
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("queued Ensure=%v", err)
		}
	case <-time.After(time.Second):
		t.Error("queued Ensure ignored its deadline while another Ensure held the lock")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !completed {
		<-second
	}
}

// Trigger cancellation when the actual installed directory has moved, rather
// than relying on a timer to land between two fast renames.
type cancelWhenMoved struct {
	context.Context
	cancel   context.CancelFunc
	template string
}

func (ctx cancelWhenMoved) Err() error {
	if _, err := os.Lstat(ctx.template); os.IsNotExist(err) {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestPromotionCancellationRestoresMovedTemplate(t *testing.T) {
	env := newTestEnv(t)
	if err := os.Mkdir(env.templateDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(env.templateDir, "installed")
	if err := os.WriteFile(sentinel, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	staging := env.templateDir + ".tmp"
	if err := os.Mkdir(staging, 0755); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := cancelWhenMoved{Context: base, cancel: cancel, template: env.templateDir}
	if err := env.m.promote(ctx, staging); !errors.Is(err, context.Canceled) {
		t.Fatalf("promote=%v", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "old" {
		t.Fatalf("old template not restored: %q, %v", got, err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("canceled promotion consumed staging: %v", err)
	}
	backups, err := filepath.Glob(env.templateDir + ".previous-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("rollback left backups: %v", backups)
	}
}
