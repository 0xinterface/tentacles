package scaleset

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
// observed, and AcquireJobs calls are recorded for acquisition tests.
type fakeSessionClient struct {
	totalAssigned int
	getErr        error
	// messages, when non-empty, are returned by GetMessage one at a
	// time (nil, nil once drained).
	messages []*scaleset.RunnerScaleSetMessage

	mu       sync.Mutex
	closed   bool
	acquired [][]int64
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, requestIDs)
	return requestIDs, nil
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

// TestNewOffline verifies New performs no network I/O: the upstream
// client constructor parses the URL and validates credentials — since
// v0.5.0 it parses the App private key, so a real throwaway key is
// required — without contacting GitHub.
func TestNewOffline(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	a, err := New(Config{
		GitHubURL:      "https://github.com/test-org",
		ClientID:       "Iv1.test",
		InstallationID: 12345678,
		PrivateKeyPEM:  string(pemBytes),
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

func TestScaleClampsDesiredCount(t *testing.T) {
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
			err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
				MessageID:  1,
				Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: tt.in},
			})
			if err != nil {
				t.Fatalf("Scale error: %v", err)
			}
			if len(fired) != 1 || fired[0] != tt.want {
				t.Errorf("ev.Desired fired with %v, want [%d]", fired, tt.want)
			}
		})
	}
}

// TestScaleNilPollReEmitsCachedCount: nil-message polls (long-poll
// timeouts) re-emit the desired count from the cached statistics, the
// idle-convergence behavior the v0.4.0 listener provided internally. A
// real message without statistics falls back to the same cache.
func TestScaleNilPollReEmitsCachedCount(t *testing.T) {
	var fired []int
	a := &Adapter{
		cfg: Config{MinRunners: 0, MaxRunners: 5},
		ev:  Events{Desired: func(n int) { fired = append(fired, n) }},
	}
	if err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID:  1,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
	}); err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if err := a.Scale(context.Background(), nil); err != nil {
		t.Fatalf("Scale(nil) error: %v", err)
	}
	if err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 2}); err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if len(fired) != 3 || fired[0] != 3 || fired[1] != 3 || fired[2] != 3 {
		t.Fatalf("ev.Desired fired %v, want [3 3 3]", fired)
	}

	// Before any statistics arrive, a nil poll must not fire anything.
	fresh := &Adapter{cfg: Config{MinRunners: 0, MaxRunners: 5}}
	if err := fresh.Scale(context.Background(), nil); err != nil {
		t.Fatalf("Scale(nil) error: %v", err)
	}
}

func TestScaleForwardsJobMessages(t *testing.T) {
	var started, ended []Job
	a := &Adapter{ev: Events{
		JobStart: func(j Job) { started = append(started, j) },
		JobEnd:   func(j Job) { ended = append(ended, j) },
	}}
	const ref = "my-org/my-repo/.github/workflows/ci.yml@refs/heads/main"
	assign := time.Now()
	finish := assign.Add(90 * time.Second)

	if err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID: 1,
		JobStartedMessages: []*scaleset.JobStarted{{
			RunnerName: "debian-host-0001-x7f3",
			JobMessageBase: scaleset.JobMessageBase{
				JobID:            "123",
				JobWorkflowRef:   ref,
				WorkflowRunID:    77,
				EventName:        "push",
				RunnerAssignTime: assign,
			},
		}},
	}); err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID: 2,
		JobCompletedMessages: []*scaleset.JobCompleted{{
			RunnerName:     "debian-host-0001-x7f3",
			Result:         "success",
			JobMessageBase: scaleset.JobMessageBase{JobWorkflowRef: ref, FinishTime: finish},
		}},
	}); err != nil {
		t.Fatalf("Scale error: %v", err)
	}

	if len(started) != 1 {
		t.Fatalf("ev.JobStart fired %d times", len(started))
	}
	s := started[0]
	if s.RunnerName != "debian-host-0001-x7f3" || s.WorkflowRef != ref || s.WorkflowRunID != 77 ||
		s.EventName != "push" || s.JobID != "123" || !s.RunnerAssignTime.Equal(assign) {
		t.Errorf("JobStart payload = %+v", s)
	}
	if len(ended) != 1 {
		t.Fatalf("ev.JobEnd fired %d times", len(ended))
	}
	e := ended[0]
	if e.Result != "success" || e.WorkflowRef != ref || !e.FinishTime.Equal(finish) {
		t.Errorf("JobEnd payload = %+v", e)
	}
}

// TestScaleAcquiresAvailableJobs: v0.5.0 makes the scaler acquire
// available jobs itself; the service never assigns unacquired jobs, so
// every request ID must reach the session client. Nil entries are
// skipped, and acquisition without a live session (Scale called outside
// Run) errors instead of silently dropping jobs.
func TestScaleAcquiresAvailableJobs(t *testing.T) {
	fake := &fakeSessionClient{}
	a := &Adapter{session: fake}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID: 5,
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 11}},
			nil,
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 12}},
		},
	}
	if err := a.Scale(context.Background(), msg); err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	fake.mu.Lock()
	got := append([][]int64(nil), fake.acquired...)
	fake.mu.Unlock()
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != 11 || got[0][1] != 12 {
		t.Fatalf("AcquireJobs calls = %v, want one call with [11 12]", got)
	}

	bare := &Adapter{}
	if err := bare.Scale(context.Background(), msg); err == nil {
		t.Fatal("Scale without a session must error on available jobs")
	}
}

func TestQueuedRefsEmitted(t *testing.T) {
	var seen [][]string
	a := &Adapter{ev: Events{Queued: func(refs []string) { seen = append(seen, refs) }}}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID: 9,
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{JobWorkflowRef: "o/r/.github/workflows/a.yml@main"}},
			{JobMessageBase: scaleset.JobMessageBase{JobWorkflowRef: "o/r/.github/workflows/b.yml@main"}},
		},
	}
	a.observeMessage(msg)
	if len(seen) != 1 || len(seen[0]) != 2 || seen[0][0] != "o/r/.github/workflows/a.yml@main" {
		t.Fatalf("Queued fired with %v", seen)
	}

	// A message with no available jobs must not fire the callback.
	a.observeMessage(&scaleset.RunnerScaleSetMessage{MessageID: 10})
	if len(seen) != 1 {
		t.Fatalf("Queued fired %d times, want 1", len(seen))
	}
}

// TestScaleSkipsNilJobEntries: the v0.5.0 listener acks only after
// Scale returns nil, so nil entries inside a message are skipped rather
// than failed — an error would leave the message unacked and redeliver
// the same poison message forever. Valid entries still fire.
func TestScaleSkipsNilJobEntries(t *testing.T) {
	var fired []string
	a := &Adapter{ev: Events{
		JobStart: func(Job) { fired = append(fired, "start") },
		JobEnd:   func(Job) { fired = append(fired, "end") },
	}}
	err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID:            3,
		JobStartedMessages:   []*scaleset.JobStarted{nil, {RunnerName: "r"}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "r", Result: "ok"}, nil},
	})
	if err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if len(fired) != 2 || fired[0] != "start" || fired[1] != "end" {
		t.Fatalf("events fired %v, want one start then one end", fired)
	}
}

// TestNilEventsSafe verifies that an Adapter with no event callbacks never
// panics on any Scaler call or session-event path.
func TestNilEventsSafe(t *testing.T) {
	a := &Adapter{cfg: Config{MinRunners: 1, MaxRunners: 4}, session: &fakeSessionClient{}}
	err := a.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "r"}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "r", Result: "ok"}},
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9}}},
	})
	if err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if err := a.Scale(context.Background(), nil); err != nil {
		t.Fatalf("Scale(nil) error: %v", err)
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
// so the daemon can gate sd_notify READY on the listener session.
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
// tentacles_last_message_id metric).
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
