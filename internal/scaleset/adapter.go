// Package scaleset adapts github.com/actions/scaleset for tentacles.
//
// This is the ONLY package in the module that imports the upstream module
// (including its listener subpackage). Upstream types must not leak past
// this package: everything the rest of the daemon sees is the Config, the
// Events callback surface, and the small Adapter API below.
//
// The adapter owns one scale set per daemon process: it resolves or creates
// the configured scale set, runs the official listener loop (which long-polls
// the scale-set message API and refreshes the session), translates listener
// callbacks into Events, and mints JIT configs. The desired runner count is
// driven exclusively by statistics.TotalAssignedJobs, clamped into
// [MinRunners, MaxRunners]; job lifecycle messages are used only for
// busy-marking and metrics.
package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/hkust/tentacles/internal/reconcile"
)

// Config carries the GitHub App credentials and scale-set settings. The
// GitHub URL already encodes the scope (organization or repository); the
// caller reads the App private key file and passes the PEM content itself.
type Config struct {
	GitHubURL      string // e.g. https://github.com or a GHES root
	ClientID       string // GitHub App client ID
	InstallationID int64  // GitHub App installation ID
	PrivateKeyPEM  string // App private key, PEM content (not a path)
	OwnerName      string // org or repo owner; informational for the adapter
	Repository     string // repo name when scope is a repository
	ScaleSetName   string // scale-set name; this is what workflows use as runs-on
	RunnerGroup    string // "default"/"Default"/"" resolve to the default group
	ExtraLabels    []string
	MinRunners     int
	MaxRunners     int
	DisableUpdate  bool   // disable runner self-update at scale-set creation
	SystemVersion  string // tentacles version reported to the upstream API
	SessionOwner   string // owner name on the message session (e.g. hostname)
}

// Events is the callback surface the rest of the daemon consumes. Every
// function is optional; nil functions are safe to call.
type Events struct {
	// SessionStarted is called once per successfully created message
	// session, before the listener loop takes over. The daemon gates
	// sd_notify READY on it (plan §11).
	SessionStarted func()
	// Desired is called with the clamped desired runner count.
	Desired func(n int)
	// JobStart is called when a runner claims a job, with the job's
	// identity and GitHub-reported timeline (plan §8: correlate
	// runnerName with a slot).
	JobStart func(Job)
	// JobEnd is called when a job finishes on a runner.
	JobEnd func(Job)
	// Queued is called with the workflow refs of jobs seen waiting in
	// the scale-set queue (JobAvailable messages). Best-effort: messages
	// can truncate, so absence of a ref does not mean the job is gone.
	Queued func(refs []string)
	// MessageID is called with the ID of every message fetched from the
	// scale-set queue (the tentacles_last_message_id metric, plan §14).
	MessageID func(id int64)
	// Session is called on every listener/session stop with the reason
	// (nil on graceful context cancellation).
	Session func(err error)
}

// Job is one workflow job as GitHub reports it in the scale-set
// message stream. Zero timestamps mean the message carried none.
type Job struct {
	RunnerName       string
	JobID            string
	WorkflowRef      string // owner/repo/.github/workflows/x.yml@ref
	WorkflowRunID    int64
	EventName        string
	QueueTime        time.Time
	RunnerAssignTime time.Time
	FinishTime       time.Time // completion only
	Result           string    // completion only
}

// Adapter wraps the upstream scale-set client and listener loop. Create it
// with New, resolve the scale set with EnsureScaleSet, then run the listener
// with Run.
type Adapter struct {
	cfg Config
	ev  Events
	log *slog.Logger

	mu         sync.Mutex
	client     *scaleset.Client
	scaleSetID int

	// newSessionFunc creates the message-session client used by Run. It is
	// a field so tests can substitute a fake listener.Client without
	// network access.
	newSessionFunc func(ctx context.Context, scaleSetID int, owner string) (listener.Client, error)
}

// New builds the adapter around a real upstream client. The upstream
// constructor only parses the config URL and stores credentials — it makes
// no network calls — so New is safe before the network is reachable.
func New(cfg Config, ev Events, log *slog.Logger) (*Adapter, error) {
	if log == nil {
		log = slog.Default()
	}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.GitHubURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       cfg.ClientID,
			InstallationID: cfg.InstallationID,
			PrivateKey:     cfg.PrivateKeyPEM,
		},
		SystemInfo: scaleset.SystemInfo{
			System:  "tentacles",
			Version: cfg.SystemVersion,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("scaleset: create client: %w", err)
	}
	a := &Adapter{
		cfg:    cfg,
		ev:     ev,
		log:    log,
		client: client,
	}
	a.newSessionFunc = a.sessionClient
	return a, nil
}

// sessionClient creates a real message-session client. MessageSessionClient
// performs one network call (session creation).
func (a *Adapter) sessionClient(ctx context.Context, scaleSetID int, owner string) (listener.Client, error) {
	return a.client.MessageSessionClient(ctx, scaleSetID, owner)
}

// EnsureScaleSet resolves the configured runner group and scale set,
// creating the scale set if it does not exist yet, and records the scale
// set ID on the client. It is idempotent: a second call reuses the existing
// scale set instead of creating a duplicate (the upstream API rejects
// duplicate names anyway).
func (a *Adapter) EnsureScaleSet(ctx context.Context) error {
	groupID := 1 // the default runner group has hardcoded ID 1 upstream.
	switch a.cfg.RunnerGroup {
	case "", "default", "Default":
	default:
		group, err := a.client.GetRunnerGroupByName(ctx, a.cfg.RunnerGroup)
		if err != nil {
			return fmt.Errorf("scaleset: resolve runner group %q: %w", a.cfg.RunnerGroup, err)
		}
		groupID = group.ID
	}

	ss, err := a.client.GetRunnerScaleSet(ctx, groupID, a.cfg.ScaleSetName)
	if err != nil {
		return fmt.Errorf("scaleset: get scale set %q: %w", a.cfg.ScaleSetName, err)
	}
	if ss == nil {
		ss, err = a.client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          a.cfg.ScaleSetName,
			RunnerGroupID: groupID,
			Labels:        a.buildLabels(),
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: a.cfg.DisableUpdate},
		})
		if err != nil {
			return fmt.Errorf("scaleset: create scale set %q: %w", a.cfg.ScaleSetName, err)
		}
	}

	a.mu.Lock()
	a.scaleSetID = ss.ID
	a.client.SetSystemInfo(scaleset.SystemInfo{
		System:     "tentacles",
		Version:    a.cfg.SystemVersion,
		ScaleSetID: ss.ID,
	})
	a.mu.Unlock()
	return nil
}

// buildLabels returns the scale-set name plus the extra labels. Upstream
// fills the label Type ("System") during creation.
func (a *Adapter) buildLabels() []scaleset.Label {
	labels := []scaleset.Label{{Name: a.cfg.ScaleSetName}}
	for _, l := range a.cfg.ExtraLabels {
		if l != "" {
			labels = append(labels, scaleset.Label{Name: l})
		}
	}
	return labels
}

// Run blocks for the lifetime of the message-session listener. It requires
// EnsureScaleSet to have completed successfully. On every return it calls
// ev.Session with the reason: nil when the context was canceled, otherwise
// the error that stopped the listener. On context cancellation the message
// session is deleted best-effort (graceful shutdown); error returns leave
// the session for the reconnect path to replace.
func (a *Adapter) Run(ctx context.Context) error {
	a.mu.Lock()
	id := a.scaleSetID
	a.mu.Unlock()
	if id == 0 {
		return errors.New("scaleset: EnsureScaleSet must be called before Run")
	}

	sessionClient, err := a.newSessionFunc(ctx, id, a.cfg.SessionOwner)
	if err != nil {
		retErr := fmt.Errorf("scaleset: create message session: %w", err)
		a.sessionEvent(retErr)
		return retErr
	}
	if a.ev.SessionStarted != nil {
		a.ev.SessionStarted()
	}
	// Observe fetched messages (IDs for the metric, queued refs as
	// admission hints) without changing the official listener loop.
	sessionClient = &watchedClient{Client: sessionClient, onMessage: a.observeMessage}

	var retErr error
	defer func() {
		if ctx.Err() != nil {
			// Graceful shutdown: delete the session best-effort and
			// never block shutdown on a dead network.
			if c, ok := sessionClient.(interface{ Close(context.Context) error }); ok {
				if err := c.Close(context.WithoutCancel(ctx)); err != nil {
					a.logger().Warn("scaleset: close message session", "err", err)
				}
			}
		}
		sessionErr := retErr
		if errors.Is(sessionErr, context.Canceled) {
			sessionErr = nil
		}
		a.sessionEvent(sessionErr)
	}()

	lst, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: id,
		MaxRunners: a.cfg.MaxRunners,
		Logger:     a.logger().WithGroup("listener"),
	})
	if err != nil {
		retErr = fmt.Errorf("scaleset: create listener: %w", err)
		return retErr
	}
	if err := lst.Run(ctx, a); err != nil {
		retErr = fmt.Errorf("scaleset: listener: %w", err)
	}
	return retErr
}

// GenerateJIT mints a JIT config for a runner with the given name and work
// folder. The returned encoded config is a secret: never log it, never write
// it anywhere but a 0600 file. Requires EnsureScaleSet to have completed.
func (a *Adapter) GenerateJIT(ctx context.Context, runnerName, workFolder string) (string, error) {
	a.mu.Lock()
	id := a.scaleSetID
	a.mu.Unlock()
	if id == 0 {
		return "", errors.New("scaleset: EnsureScaleSet must be called before GenerateJIT")
	}
	cfg, err := a.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: workFolder,
	}, id)
	if err != nil {
		return "", fmt.Errorf("scaleset: generate JIT config: %w", err)
	}
	return cfg.EncodedJITConfig, nil
}

// ScaleSetID returns the resolved scale set ID, or 0 before EnsureScaleSet.
func (a *Adapter) ScaleSetID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scaleSetID
}

// sessionEvent reports a session stop to ev.Session, tolerating a nil
// callback.
func (a *Adapter) sessionEvent(err error) {
	if a.ev.Session != nil {
		a.ev.Session(err)
	}
}

// HandleDesiredRunnerCount implements listener.Scaler: it clamps the
// statistics-driven desired count into [MinRunners, MaxRunners] and emits it
// via ev.Desired. The clamped value is returned so the listener can record
// it.
func (a *Adapter) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	d := reconcile.Clamp(count, a.cfg.MinRunners, a.cfg.MaxRunners)
	if a.ev.Desired != nil {
		a.ev.Desired(d)
	}
	return d, nil
}

// HandleJobStarted implements listener.Scaler. It forwards the job's
// identity and timeline so the daemon can attribute resource usage to a
// workflow (plan §8: use lifecycle messages to correlate runnerName).
func (a *Adapter) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	if jobInfo == nil {
		return errors.New("scaleset: nil JobStarted message")
	}
	if a.ev.JobStart != nil {
		a.ev.JobStart(job(jobInfo.JobMessageBase, jobInfo.RunnerName, ""))
	}
	return nil
}

// HandleJobCompleted implements listener.Scaler.
func (a *Adapter) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	if jobInfo == nil {
		return errors.New("scaleset: nil JobCompleted message")
	}
	if a.ev.JobEnd != nil {
		a.ev.JobEnd(job(jobInfo.JobMessageBase, jobInfo.RunnerName, jobInfo.Result))
	}
	return nil
}

// job maps an upstream job message onto the local Job view.
func job(base scaleset.JobMessageBase, runnerName, result string) Job {
	return Job{
		RunnerName:       runnerName,
		JobID:            base.JobID,
		WorkflowRef:      base.JobWorkflowRef,
		WorkflowRunID:    base.WorkflowRunID,
		EventName:        base.EventName,
		QueueTime:        base.QueueTime,
		RunnerAssignTime: base.RunnerAssignTime,
		FinishTime:       base.FinishTime,
		Result:           result,
	}
}

// observeMessage reports one fetched queue message to the event surface:
// its ID for the metric, and the workflow refs of jobs still waiting in
// the queue (admission hints).
func (a *Adapter) observeMessage(msg *scaleset.RunnerScaleSetMessage) {
	if a.ev.MessageID != nil {
		a.ev.MessageID(int64(msg.MessageID))
	}
	if a.ev.Queued == nil || len(msg.JobAvailableMessages) == 0 {
		return
	}
	refs := make([]string, 0, len(msg.JobAvailableMessages))
	for _, ja := range msg.JobAvailableMessages {
		if ja != nil && ja.JobWorkflowRef != "" {
			refs = append(refs, ja.JobWorkflowRef)
		}
	}
	if len(refs) > 0 {
		a.ev.Queued(refs)
	}
}

// logger returns the adapter logger, defaulting to the process logger.
func (a *Adapter) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}

// watchedClient wraps a listener.Client so the adapter can observe the
// ID of every fetched message. Close is forwarded explicitly: it is not
// part of the listener.Client interface, so embedding alone would hide
// the underlying session delete.
type watchedClient struct {
	listener.Client
	onMessage func(*scaleset.RunnerScaleSetMessage)
}

func (w *watchedClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := w.Client.GetMessage(ctx, lastMessageID, maxCapacity)
	if err == nil && msg != nil && w.onMessage != nil {
		w.onMessage(msg)
	}
	return msg, err
}

func (w *watchedClient) Close(ctx context.Context) error {
	if c, ok := w.Client.(interface{ Close(context.Context) error }); ok {
		return c.Close(ctx)
	}
	return nil
}
