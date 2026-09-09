package scaleset

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

// fakeSessionClient implements listener.Client offline: it long-polls with
// (nil, nil) messages and reports a fixed TotalAssignedJobs. It also
// implements Close so the adapter's graceful-shutdown session delete can be
// observed.
type fakeSessionClient struct {
	totalAssigned int
	getErr        error
	// messages, when non-empty, are returned by GetMessage one at a
	// time (nil, nil once drained).
	messages []*scaleset.RunnerScaleSetMessage

	mu     sync.Mutex
	closed bool
}

func (f *fakeSessionClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.messages) > 0 {
		msg := f.messages[0]
		f.messages = f.messages[1:]
		return msg, nil
	}
	return nil, nil
}

func (f *fakeSessionClient) DeleteMessage(ctx context.Context, messageID int) error { return nil }

func (f *fakeSessionClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	return nil, nil
}

func (f *fakeSessionClient) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{
		SessionID:  uuid.UUID{1: 1},
		OwnerName:  "test-host",
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: f.totalAssigned},
	}
}

func (f *fakeSessionClient) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSessionClient) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeSession returns a newSessionFunc that hands out the given client.
func fakeSession(c listener.Client) func(ctx context.Context, scaleSetID int, owner string) (listener.Client, error) {
	return func(context.Context, int, string) (listener.Client, error) { return c, nil }
}

// TestNewOffline verifies New performs no network I/O: the upstream client
// constructor only parses the URL and stores credentials, so a dummy PEM is
// fine.
func TestNewOffline(t *testing.T) {
	a, err := New(Config{
		GitHubURL:      "https://github.com/test-org",
		ClientID:       "Iv1.test",
		InstallationID: 12345678,
		PrivateKeyPEM:  "-----BEGIN RSA PRIVATE KEY-----\ndummy\n-----END RSA PRIVATE KEY-----",
		ScaleSetName:   "debian-host",
		OwnerName:      "test-org",
		MaxRunners:     4,
		SystemVersion:  "0.1.0",
		SessionOwner:   "test-host",
	}, Events{}, discardLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if a.client == nil {
		t.Fatal("New did not build an upstream client")
	}
	if a.ScaleSetID() != 0 {
		t.Fatalf("ScaleSetID() = %d before EnsureScaleSet, want 0", a.ScaleSetID())
	}
}

func TestHandleDesiredRunnerCountClamps(t *testing.T) {
	tests := []struct {
		name string
		min  int
		max  int
		in   int
		want int
	}{
		{"below min", 2, 5, 0, 2},
		{"at min", 2, 5, 2, 2},
		{"in range", 2, 5, 4, 4},
		{"at max", 2, 5, 5, 5},
		{"above max", 2, 5, 9, 5},
		{"min zero idle", 0, 3, 0, 0},
		{"min zero active", 0, 3, 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fired []int
			a := &Adapter{
				cfg: Config{MinRunners: tt.min, MaxRunners: tt.max},
				ev:  Events{Desired: func(n int) { fired = append(fired, n) }},
			}
			got, err := a.HandleDesiredRunnerCount(context.Background(), tt.in)
			if err != nil {
				t.Fatalf("HandleDesiredRunnerCount error: %v", err)
			}
			if got != tt.want {
				t.Errorf("returned %d, want %d", got, tt.want)
			}
			if len(fired) != 1 || fired[0] != tt.want {
				t.Errorf("ev.Desired fired with %v, want [%d]", fired, tt.want)
			}
		})
	}
}

func TestHandleJobMessages(t *testing.T) {
	var started []string
	var ended []struct{ name, result string }
	a := &Adapter{ev: Events{
		JobStart: func(name string) { started = append(started, name) },
		JobEnd:   func(name, result string) { ended = append(ended, struct{ name, result string }{name, result}) },
	}}

	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "debian-host-0001-x7f3"}); err != nil {
		t.Fatalf("HandleJobStarted error: %v", err)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "debian-host-0001-x7f3", Result: "success"}); err != nil {
		t.Fatalf("HandleJobCompleted error: %v", err)
	}

	if len(started) != 1 || started[0] != "debian-host-0001-x7f3" {
		t.Errorf("ev.JobStart fired with %v, want [debian-host-0001-x7f3]", started)
	}
	if len(ended) != 1 || ended[0].name != "debian-host-0001-x7f3" || ended[0].result != "success" {
		t.Errorf("ev.JobEnd fired with %v, want [{debian-host-0001-x7f3 success}]", ended)
	}
}

func TestNilMessageRejected(t *testing.T) {
	a := &Adapter{}
	if err := a.HandleJobStarted(context.Background(), nil); err == nil {
		t.Error("nil JobStarted must error, got nil")
	}
	if err := a.HandleJobCompleted(context.Background(), nil); err == nil {
		t.Error("nil JobCompleted must error, got nil")
	}
}

// TestNilEventsSafe verifies that an Adapter with no event callbacks never
// panics on any Scaler call or session-event path.
func TestNilEventsSafe(t *testing.T) {
	a := &Adapter{cfg: Config{MinRunners: 1, MaxRunners: 4}}
	if _, err := a.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatalf("HandleDesiredRunnerCount error: %v", err)
	}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatalf("HandleJobStarted error: %v", err)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r", Result: "ok"}); err != nil {
		t.Fatalf("HandleJobCompleted error: %v", err)
	}

	// Session-event paths must not panic with a nil ev.Session.
	a.log = discardLogger()
	a.scaleSetID = 1
	a.newSessionFunc = func(context.Context, int, string) (listener.Client, error) {
		return nil, errors.New("no network")
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("expected session-create error")
	}

	a.newSessionFunc = fakeSession(&fakeSessionClient{totalAssigned: 0, getErr: errors.New("poll failed")})
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("expected listener error")
	}
}

func TestRunRequiresEnsureScaleSet(t *testing.T) {
	a := &Adapter{cfg: Config{}, log: discardLogger()}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("Run without EnsureScaleSet must error, got nil")
	}
}

func TestGenerateJITRequiresEnsureScaleSet(t *testing.T) {
	a := &Adapter{cfg: Config{}}
	if _, err := a.GenerateJIT(context.Background(), "r", "_work"); err == nil {
		t.Fatal("GenerateJIT without EnsureScaleSet must error, got nil")
	}
}

// TestRunListenerLoopOffline drives the real upstream listener loop against
// a fake client: initial session statistics fire the first desired count,
// nil-message polls keep firing it, cancellation stops the loop, ev.Session
// is called with nil, and the message session is closed (graceful shutdown).
func TestRunListenerLoopOffline(t *testing.T) {
	fake := &fakeSessionClient{totalAssigned: 7}
	a := &Adapter{
		cfg:            Config{MinRunners: 1, MaxRunners: 5, SessionOwner: "test-host"},
		log:            discardLogger(),
		scaleSetID:     42,
		newSessionFunc: fakeSession(fake),
	}

	var mu sync.Mutex
	var first bool
	var values []int
	firstCh := make(chan int, 1)
	a.ev.Desired = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		values = append(values, n)
		if !first {
			first = true
			firstCh <- n
		}
	}
	sessionErrCh := make(chan error, 1)
	a.ev.Session = func(err error) { sessionErrCh <- err }

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	// The initial session statistics must produce the first clamped count.
	select {
	case n := <-firstCh:
		if n != 5 {
			t.Fatalf("first desired = %d, want 5 (7 clamped to max 5)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener never reported a desired count")
	}

	cancel()

	select {
	case err := <-runErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want a wrapped context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	mu.Lock()
	if len(values) < 1 {
		t.Error("ev.Desired was never called")
	}
	for i, v := range values {
		if v != 5 {
			t.Errorf("ev.Desired call %d = %d, want 5", i, v)
		}
	}
	mu.Unlock()

	// ev.Session fires in Run's defer before Run returns, so this read
	// cannot block once runErrCh has produced a value.
	select {
	case err := <-sessionErrCh:
		if err != nil {
			t.Fatalf("ev.Session got %v, want nil on clean cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ev.Session was not called after Run returned")
	}

	if !fake.isClosed() {
		t.Error("message session was not closed on graceful shutdown")
	}
}

// TestRunCallsSessionOnListenerError verifies listener failures surface both
// as Run's error and through ev.Session, and that the session is NOT deleted
// (the reconnect path creates a fresh one).
func TestRunCallsSessionOnListenerError(t *testing.T) {
	boom := errors.New("poll failed")
	fake := &fakeSessionClient{totalAssigned: 1, getErr: boom}
	a := &Adapter{
		cfg:            Config{MinRunners: 1, MaxRunners: 4, SessionOwner: "test-host"},
		log:            discardLogger(),
		scaleSetID:     1,
		newSessionFunc: fakeSession(fake),
	}
	var sessionErr error
	a.ev.Session = func(err error) { sessionErr = err }

	err := a.Run(context.Background())
	if err == nil {
		t.Fatal("Run must return the listener error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Run error %v does not wrap %v", err, boom)
	}
	if sessionErr == nil {
		t.Fatal("ev.Session must be called with the error")
	}
	if !errors.Is(sessionErr, boom) {
		t.Fatalf("ev.Session error %v does not wrap %v", sessionErr, boom)
	}
	if fake.isClosed() {
		t.Error("message session must NOT be closed on a non-cancel error return")
	}
}

// TestRunSessionCreateError verifies a failed session creation surfaces via
// ev.Session and Run's error.
func TestRunSessionCreateError(t *testing.T) {
	boom := errors.New("no network")
	a := &Adapter{
		cfg:        Config{SessionOwner: "test-host"},
		log:        discardLogger(),
		scaleSetID: 1,
		newSessionFunc: func(context.Context, int, string) (listener.Client, error) {
			return nil, boom
		},
	}
	var sessionErr error
	a.ev.Session = func(err error) { sessionErr = err }

	err := a.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run error %v does not wrap %v", err, boom)
	}
	if !errors.Is(sessionErr, boom) {
		t.Fatalf("ev.Session error %v does not wrap %v", sessionErr, boom)
	}
}

// TestRunEmitsSessionStarted: the adapter reports session establishment
// (plan §11 gates sd_notify READY on the listener session being started).
func TestRunEmitsSessionStarted(t *testing.T) {
	fake := &fakeSessionClient{totalAssigned: 1}
	a := &Adapter{
		cfg:            Config{MinRunners: 0, MaxRunners: 5, SessionOwner: "test-host"},
		log:            discardLogger(),
		scaleSetID:     42,
		newSessionFunc: fakeSession(fake),
	}
	startedCh := make(chan struct{}, 1)
	a.ev.SessionStarted = func() { startedCh <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("SessionStarted never fired after session creation")
	}
	cancel()
	<-runErr
}

// TestRunSessionCreateErrorDoesNotEmitSessionStarted: readiness is only
// reported for a session that actually exists.
func TestRunSessionCreateErrorDoesNotEmitSessionStarted(t *testing.T) {
	a := &Adapter{
		cfg:        Config{MinRunners: 0, MaxRunners: 5},
		log:        discardLogger(),
		scaleSetID: 42,
		newSessionFunc: func(context.Context, int, string) (listener.Client, error) {
			return nil, errors.New("session create boom")
		},
	}
	fired := make(chan struct{}, 1)
	a.ev.SessionStarted = func() { fired <- struct{}{} }
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("expected Run to fail on session-create error")
	}
	select {
	case <-fired:
		t.Fatal("SessionStarted fired despite failed session creation")
	default:
	}
}

// TestRunReportsMessageIDs: the adapter wraps the session client so
// every fetched message ID reaches ev.MessageID (the
// tentacles_last_message_id metric, plan §14).
func TestRunReportsMessageIDs(t *testing.T) {
	fake := &fakeSessionClient{
		totalAssigned: 1,
		messages: []*scaleset.RunnerScaleSetMessage{{
			MessageID:  7,
			Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		}},
	}
	a := &Adapter{
		cfg:            Config{MinRunners: 0, MaxRunners: 5, SessionOwner: "test-host"},
		log:            discardLogger(),
		scaleSetID:     42,
		newSessionFunc: fakeSession(fake),
	}
	idCh := make(chan int64, 1)
	a.ev.MessageID = func(id int64) { idCh <- id }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	select {
	case id := <-idCh:
		if id != 7 {
			t.Fatalf("MessageID = %d, want 7", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MessageID never fired for the fetched message")
	}
}
