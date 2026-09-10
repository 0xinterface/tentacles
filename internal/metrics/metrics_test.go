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
	r.IncAdmissionHolds()
	r.ObserveJobCPU(45)
	r.ObserveJobWall(90)
	r.SetJobPeakMem(536870912)
	r.ObserveSlotStart(0.05)
	r.ObserveSlotStart(2.0)

	want := `# HELP tentacles_acquire_failures_total Total number of times a runner failed to acquire its assigned job.
# TYPE tentacles_acquire_failures_total counter
tentacles_acquire_failures_total 1
# HELP tentacles_actual_runners Current number of runner slots by lifecycle state.
# TYPE tentacles_actual_runners gauge
tentacles_actual_runners{state="busy"} 1
tentacles_actual_runners{state="idle"} 2
# HELP tentacles_admission_holds_total Total number of slot starts held back by the admission gate.
# TYPE tentacles_admission_holds_total counter
tentacles_admission_holds_total 1
# HELP tentacles_desired_runners Current desired number of runner slots.
# TYPE tentacles_desired_runners gauge
tentacles_desired_runners 3
# HELP tentacles_job_cpu_seconds CPU seconds consumed by finished jobs.
# TYPE tentacles_job_cpu_seconds histogram
tentacles_job_cpu_seconds_bucket{le="1"} 0
tentacles_job_cpu_seconds_bucket{le="5"} 0
tentacles_job_cpu_seconds_bucket{le="10"} 0
tentacles_job_cpu_seconds_bucket{le="30"} 0
tentacles_job_cpu_seconds_bucket{le="60"} 1
tentacles_job_cpu_seconds_bucket{le="120"} 1
tentacles_job_cpu_seconds_bucket{le="300"} 1
tentacles_job_cpu_seconds_bucket{le="600"} 1
tentacles_job_cpu_seconds_bucket{le="1800"} 1
tentacles_job_cpu_seconds_bucket{le="3600"} 1
tentacles_job_cpu_seconds_bucket{le="+Inf"} 1
tentacles_job_cpu_seconds_sum 45
tentacles_job_cpu_seconds_count 1
# HELP tentacles_job_wall_seconds Wall-clock seconds of finished jobs.
# TYPE tentacles_job_wall_seconds histogram
tentacles_job_wall_seconds_bucket{le="5"} 0
tentacles_job_wall_seconds_bucket{le="15"} 0
tentacles_job_wall_seconds_bucket{le="30"} 0
tentacles_job_wall_seconds_bucket{le="60"} 0
tentacles_job_wall_seconds_bucket{le="120"} 1
tentacles_job_wall_seconds_bucket{le="300"} 1
tentacles_job_wall_seconds_bucket{le="600"} 1
tentacles_job_wall_seconds_bucket{le="1200"} 1
tentacles_job_wall_seconds_bucket{le="1800"} 1
tentacles_job_wall_seconds_bucket{le="3600"} 1
tentacles_job_wall_seconds_bucket{le="+Inf"} 1
tentacles_job_wall_seconds_sum 90
tentacles_job_wall_seconds_count 1
# HELP tentacles_jobs_completed_total Total number of completed jobs, by result.
# TYPE tentacles_jobs_completed_total counter
tentacles_jobs_completed_total{result="failure"} 1
tentacles_jobs_completed_total{result="success"} 1
# HELP tentacles_jobs_started_total Total number of started jobs.
# TYPE tentacles_jobs_started_total counter
tentacles_jobs_started_total 2
# HELP tentacles_last_job_peak_memory_bytes Peak memory of the most recent finished job.
# TYPE tentacles_last_job_peak_memory_bytes gauge
tentacles_last_job_peak_memory_bytes 5.36870912e+08
# HELP tentacles_last_message_id ID of the last processed scale-set message.
# TYPE tentacles_last_message_id gauge
tentacles_last_message_id 42
# HELP tentacles_listener_errors_total Total number of scale-set listener loop errors.
# TYPE tentacles_listener_errors_total counter
tentacles_listener_errors_total 1
# HELP tentacles_slot_start_seconds Time to start a runner slot.
# TYPE tentacles_slot_start_seconds histogram
tentacles_slot_start_seconds_bucket{le="0.05"} 1
tentacles_slot_start_seconds_bucket{le="0.1"} 1
tentacles_slot_start_seconds_bucket{le="0.25"} 1
tentacles_slot_start_seconds_bucket{le="0.5"} 1
tentacles_slot_start_seconds_bucket{le="1"} 1
tentacles_slot_start_seconds_bucket{le="2.5"} 2
tentacles_slot_start_seconds_bucket{le="5"} 2
tentacles_slot_start_seconds_bucket{le="10"} 2
tentacles_slot_start_seconds_bucket{le="30"} 2
tentacles_slot_start_seconds_bucket{le="60"} 2
tentacles_slot_start_seconds_bucket{le="+Inf"} 2
tentacles_slot_start_seconds_sum 2.05
tentacles_slot_start_seconds_count 2
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
	want := `# HELP tentacles_acquire_failures_total Total number of times a runner failed to acquire its assigned job.
# TYPE tentacles_acquire_failures_total counter
tentacles_acquire_failures_total 0
# HELP tentacles_admission_holds_total Total number of slot starts held back by the admission gate.
# TYPE tentacles_admission_holds_total counter
tentacles_admission_holds_total 0
# HELP tentacles_desired_runners Current desired number of runner slots.
# TYPE tentacles_desired_runners gauge
tentacles_desired_runners 0
# HELP tentacles_job_cpu_seconds CPU seconds consumed by finished jobs.
# TYPE tentacles_job_cpu_seconds histogram
tentacles_job_cpu_seconds_bucket{le="1"} 0
tentacles_job_cpu_seconds_bucket{le="5"} 0
tentacles_job_cpu_seconds_bucket{le="10"} 0
tentacles_job_cpu_seconds_bucket{le="30"} 0
tentacles_job_cpu_seconds_bucket{le="60"} 0
tentacles_job_cpu_seconds_bucket{le="120"} 0
tentacles_job_cpu_seconds_bucket{le="300"} 0
tentacles_job_cpu_seconds_bucket{le="600"} 0
tentacles_job_cpu_seconds_bucket{le="1800"} 0
tentacles_job_cpu_seconds_bucket{le="3600"} 0
tentacles_job_cpu_seconds_bucket{le="+Inf"} 0
tentacles_job_cpu_seconds_sum 0
tentacles_job_cpu_seconds_count 0
# HELP tentacles_job_wall_seconds Wall-clock seconds of finished jobs.
# TYPE tentacles_job_wall_seconds histogram
tentacles_job_wall_seconds_bucket{le="5"} 0
tentacles_job_wall_seconds_bucket{le="15"} 0
tentacles_job_wall_seconds_bucket{le="30"} 0
tentacles_job_wall_seconds_bucket{le="60"} 0
tentacles_job_wall_seconds_bucket{le="120"} 0
tentacles_job_wall_seconds_bucket{le="300"} 0
tentacles_job_wall_seconds_bucket{le="600"} 0
tentacles_job_wall_seconds_bucket{le="1200"} 0
tentacles_job_wall_seconds_bucket{le="1800"} 0
tentacles_job_wall_seconds_bucket{le="3600"} 0
tentacles_job_wall_seconds_bucket{le="+Inf"} 0
tentacles_job_wall_seconds_sum 0
tentacles_job_wall_seconds_count 0
# HELP tentacles_jobs_started_total Total number of started jobs.
# TYPE tentacles_jobs_started_total counter
tentacles_jobs_started_total 0
# HELP tentacles_last_job_peak_memory_bytes Peak memory of the most recent finished job.
# TYPE tentacles_last_job_peak_memory_bytes gauge
tentacles_last_job_peak_memory_bytes 0
# HELP tentacles_last_message_id ID of the last processed scale-set message.
# TYPE tentacles_last_message_id gauge
tentacles_last_message_id 0
# HELP tentacles_listener_errors_total Total number of scale-set listener loop errors.
# TYPE tentacles_listener_errors_total counter
tentacles_listener_errors_total 0
# HELP tentacles_slot_start_seconds Time to start a runner slot.
# TYPE tentacles_slot_start_seconds histogram
tentacles_slot_start_seconds_bucket{le="0.05"} 0
tentacles_slot_start_seconds_bucket{le="0.1"} 0
tentacles_slot_start_seconds_bucket{le="0.25"} 0
tentacles_slot_start_seconds_bucket{le="0.5"} 0
tentacles_slot_start_seconds_bucket{le="1"} 0
tentacles_slot_start_seconds_bucket{le="2.5"} 0
tentacles_slot_start_seconds_bucket{le="5"} 0
tentacles_slot_start_seconds_bucket{le="10"} 0
tentacles_slot_start_seconds_bucket{le="30"} 0
tentacles_slot_start_seconds_bucket{le="60"} 0
tentacles_slot_start_seconds_bucket{le="+Inf"} 0
tentacles_slot_start_seconds_sum 0
tentacles_slot_start_seconds_count 0
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
	if !strings.Contains(body, "tentacles_actual_runners{state=\"busy\"} 3") {
		t.Errorf("busy state missing:\n%s", body)
	}
	if !strings.Contains(body, "tentacles_actual_runners{state=\"idle\"} 0") {
		t.Errorf("idle state missing or stale:\n%s", body)
	}
	if strings.Count(body, "tentacles_actual_runners{") != 2 {
		t.Errorf("want exactly 2 actual_runners samples:\n%s", body)
	}
}

func TestJobsCompletedUnknownResult(t *testing.T) {
	r := NewRegistry()
	r.IncJobsCompleted("canceled")
	r.IncJobsCompleted("success")
	body := r.render()
	if !strings.Contains(body, "tentacles_jobs_completed_total{result=\"canceled\"} 1") {
		t.Errorf("canceled result missing:\n%s", body)
	}
	if !strings.Contains(body, "tentacles_jobs_completed_total{result=\"success\"} 1") {
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
		"tentacles_slot_start_seconds_bucket{le=\"0.05\"} 1": "",
		"tentacles_slot_start_seconds_bucket{le=\"1\"} 1":    "",
		"tentacles_slot_start_seconds_bucket{le=\"2.5\"} 2":  "",
		"tentacles_slot_start_seconds_bucket{le=\"30\"} 2":   "",
		"tentacles_slot_start_seconds_bucket{le=\"60\"} 2":   "",
		"tentacles_slot_start_seconds_bucket{le=\"+Inf\"} 3": "",
		"tentacles_slot_start_seconds_sum 63.03":             "",
		"tentacles_slot_start_seconds_count 3":               "",
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
	if !strings.Contains(body, "tentacles_jobs_started_total 1000") {
		t.Errorf("jobs_started_total != 1000 after concurrent increments:\n%s", body)
	}
	if !strings.Contains(body, "tentacles_jobs_completed_total{result=\"success\"} 1000") {
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
