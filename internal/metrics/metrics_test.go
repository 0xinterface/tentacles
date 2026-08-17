package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRenderExact(t *testing.T) {
	r := NewRegistry()
	r.SetDesired(3)
	r.SetActual("idle", 2)
	r.SetActual("busy", 1)
	r.IncJobsStarted()
	r.IncJobsStarted()
	r.IncJobsCompleted("success")
	r.IncJobsCompleted("failure")
	r.IncAcquireFailures()
	r.IncListenerErrors()
	r.SetLastMessageID(42)
	r.ObserveSlotStart(0.05)
	r.ObserveSlotStart(2.0)

	want := `# HELP gh_runnerd_acquire_failures_total Total number of times a runner failed to acquire its assigned job.
# TYPE gh_runnerd_acquire_failures_total counter
gh_runnerd_acquire_failures_total 1
# HELP gh_runnerd_actual_runners Current number of runner slots by lifecycle state.
# TYPE gh_runnerd_actual_runners gauge
gh_runnerd_actual_runners{state="busy"} 1
gh_runnerd_actual_runners{state="idle"} 2
# HELP gh_runnerd_desired_runners Current desired number of runner slots.
# TYPE gh_runnerd_desired_runners gauge
gh_runnerd_desired_runners 3
# HELP gh_runnerd_jobs_completed_total Total number of completed jobs, by result.
# TYPE gh_runnerd_jobs_completed_total counter
gh_runnerd_jobs_completed_total{result="failure"} 1
gh_runnerd_jobs_completed_total{result="success"} 1
# HELP gh_runnerd_jobs_started_total Total number of started jobs.
# TYPE gh_runnerd_jobs_started_total counter
gh_runnerd_jobs_started_total 2
# HELP gh_runnerd_last_message_id ID of the last processed scale-set message.
# TYPE gh_runnerd_last_message_id gauge
gh_runnerd_last_message_id 42
# HELP gh_runnerd_listener_errors_total Total number of scale-set listener loop errors.
# TYPE gh_runnerd_listener_errors_total counter
gh_runnerd_listener_errors_total 1
# HELP gh_runnerd_slot_start_seconds Time to start a runner slot.
# TYPE gh_runnerd_slot_start_seconds histogram
gh_runnerd_slot_start_seconds_bucket{le="0.05"} 1
gh_runnerd_slot_start_seconds_bucket{le="0.1"} 1
gh_runnerd_slot_start_seconds_bucket{le="0.25"} 1
gh_runnerd_slot_start_seconds_bucket{le="0.5"} 1
gh_runnerd_slot_start_seconds_bucket{le="1"} 1
gh_runnerd_slot_start_seconds_bucket{le="2.5"} 2
gh_runnerd_slot_start_seconds_bucket{le="5"} 2
gh_runnerd_slot_start_seconds_bucket{le="10"} 2
gh_runnerd_slot_start_seconds_bucket{le="30"} 2
gh_runnerd_slot_start_seconds_bucket{le="60"} 2
gh_runnerd_slot_start_seconds_bucket{le="+Inf"} 2
gh_runnerd_slot_start_seconds_sum 2.05
gh_runnerd_slot_start_seconds_count 2
`
	if got := r.render(); got != want {
		t.Errorf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderEmptyRegistry locks in the fixed-series behavior: counters and
// gauges exist from the first scrape at zero, the histogram exists empty,
// and label-set families with no members are absent.
func TestRenderEmptyRegistry(t *testing.T) {
	r := NewRegistry()
	want := `# HELP gh_runnerd_acquire_failures_total Total number of times a runner failed to acquire its assigned job.
# TYPE gh_runnerd_acquire_failures_total counter
gh_runnerd_acquire_failures_total 0
# HELP gh_runnerd_desired_runners Current desired number of runner slots.
# TYPE gh_runnerd_desired_runners gauge
gh_runnerd_desired_runners 0
# HELP gh_runnerd_jobs_started_total Total number of started jobs.
# TYPE gh_runnerd_jobs_started_total counter
gh_runnerd_jobs_started_total 0
# HELP gh_runnerd_last_message_id ID of the last processed scale-set message.
# TYPE gh_runnerd_last_message_id gauge
gh_runnerd_last_message_id 0
# HELP gh_runnerd_listener_errors_total Total number of scale-set listener loop errors.
# TYPE gh_runnerd_listener_errors_total counter
gh_runnerd_listener_errors_total 0
# HELP gh_runnerd_slot_start_seconds Time to start a runner slot.
# TYPE gh_runnerd_slot_start_seconds histogram
gh_runnerd_slot_start_seconds_bucket{le="0.05"} 0
gh_runnerd_slot_start_seconds_bucket{le="0.1"} 0
gh_runnerd_slot_start_seconds_bucket{le="0.25"} 0
gh_runnerd_slot_start_seconds_bucket{le="0.5"} 0
gh_runnerd_slot_start_seconds_bucket{le="1"} 0
gh_runnerd_slot_start_seconds_bucket{le="2.5"} 0
gh_runnerd_slot_start_seconds_bucket{le="5"} 0
gh_runnerd_slot_start_seconds_bucket{le="10"} 0
gh_runnerd_slot_start_seconds_bucket{le="30"} 0
gh_runnerd_slot_start_seconds_bucket{le="60"} 0
gh_runnerd_slot_start_seconds_bucket{le="+Inf"} 0
gh_runnerd_slot_start_seconds_sum 0
gh_runnerd_slot_start_seconds_count 0
`
	if got := r.render(); got != want {
		t.Errorf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestActualRunnersStateOverwrite verifies SetActual overwrites and that
// states with a zero count are still emitted (the app reports every state
// every tick).
func TestActualRunnersStateOverwrite(t *testing.T) {
	r := NewRegistry()
	r.SetActual("idle", 1)
	r.SetActual("idle", 0)
	r.SetActual("busy", 3)
	body := r.render()
	if !strings.Contains(body, "gh_runnerd_actual_runners{state=\"busy\"} 3") {
		t.Errorf("busy state missing:\n%s", body)
	}
	if !strings.Contains(body, "gh_runnerd_actual_runners{state=\"idle\"} 0") {
		t.Errorf("idle state missing or stale:\n%s", body)
	}
	if strings.Count(body, "gh_runnerd_actual_runners{") != 2 {
		t.Errorf("want exactly 2 actual_runners samples:\n%s", body)
	}
}

func TestJobsCompletedUnknownResult(t *testing.T) {
	r := NewRegistry()
	r.IncJobsCompleted("canceled")
	r.IncJobsCompleted("success")
	body := r.render()
	if !strings.Contains(body, "gh_runnerd_jobs_completed_total{result=\"canceled\"} 1") {
		t.Errorf("canceled result missing:\n%s", body)
	}
	if !strings.Contains(body, "gh_runnerd_jobs_completed_total{result=\"success\"} 1") {
		t.Errorf("success result missing:\n%s", body)
	}
}

func TestHistogramBuckets(t *testing.T) {
	r := NewRegistry()
	// 2.0 lands in le=2.5 (and every wider bucket); 0.03 lands only in
	// +Inf; 61 lands only in +Inf.
	r.ObserveSlotStart(2.0)
	r.ObserveSlotStart(0.03)
	r.ObserveSlotStart(61)
	body := r.render()
	checks := map[string]string{
		"gh_runnerd_slot_start_seconds_bucket{le=\"0.05\"} 1": "",
		"gh_runnerd_slot_start_seconds_bucket{le=\"1\"} 1":    "",
		"gh_runnerd_slot_start_seconds_bucket{le=\"2.5\"} 2":  "",
		"gh_runnerd_slot_start_seconds_bucket{le=\"30\"} 2":   "",
		"gh_runnerd_slot_start_seconds_bucket{le=\"60\"} 2":   "",
		"gh_runnerd_slot_start_seconds_bucket{le=\"+Inf\"} 3": "",
		"gh_runnerd_slot_start_seconds_sum 63.03":             "",
		"gh_runnerd_slot_start_seconds_count 3":               "",
	}
	for line := range checks {
		if !strings.Contains(body, line) {
			t.Errorf("missing or wrong histogram line %q:\n%s", line, body)
		}
	}
}

func TestConcurrentIncrements(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r.IncJobsStarted()
				r.IncJobsCompleted("success")
			}
		}()
	}
	wg.Wait()

	body := r.render()
	if !strings.Contains(body, "gh_runnerd_jobs_started_total 1000") {
		t.Errorf("jobs_started_total != 1000 after concurrent increments:\n%s", body)
	}
	if !strings.Contains(body, "gh_runnerd_jobs_completed_total{result=\"success\"} 1000") {
		t.Errorf("jobs_completed_total != 1000 after concurrent increments:\n%s", body)
	}
}

func TestHandlerServesTextFormat(t *testing.T) {
	r := NewRegistry()
	r.SetDesired(2)
	r.SetActual("idle", 1)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got, want := rec.Body.String(), r.render(); got != want {
		t.Errorf("handler body differs from render():\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("escapeLabel = %q", got)
	}
}
