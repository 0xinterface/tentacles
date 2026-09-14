package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRenderSeparatesPools(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterPool("org-b")
	registry.RegisterPool("org-a")
	registry.SetHostMaxRunners(4)
	registry.SetDesired("org-a", 3)
	registry.SetActual("org-a", "busy", 1)
	registry.SetActual("org-b", "idle", 2)
	registry.IncJobsStarted("org-a")
	registry.IncJobsCompleted("org-b", "success")
	registry.IncAcquireFailures("org-a")
	registry.IncListenerErrors("org-b")
	registry.SetLastMessageID("org-b", 42)
	registry.IncAdmissionHolds("org-a")
	registry.ObserveJobCPU("org-a", 45)
	registry.ObserveJobWall("org-a", 90)
	registry.SetJobPeakMem("org-a", 536870912)
	registry.ObserveSlotStart("org-b", 2)

	body := registry.render()
	for _, want := range []string{
		`tentacles_desired_runners{pool="org-a"} 3`,
		`tentacles_desired_runners{pool="org-b"} 0`,
		`tentacles_actual_runners{pool="org-a",state="busy"} 1`,
		`tentacles_actual_runners{pool="org-b",state="idle"} 2`,
		`tentacles_host_max_runners 4`,
		`tentacles_jobs_started_total{pool="org-a"} 1`,
		`tentacles_jobs_started_total{pool="org-b"} 0`,
		`tentacles_jobs_completed_total{pool="org-b",result="success"} 1`,
		`tentacles_acquire_failures_total{pool="org-a"} 1`,
		`tentacles_listener_errors_total{pool="org-b"} 1`,
		`tentacles_last_message_id{pool="org-b"} 42`,
		`tentacles_admission_holds_total{pool="org-a"} 1`,
		`tentacles_job_cpu_seconds_count{pool="org-a"} 1`,
		`tentacles_job_wall_seconds_count{pool="org-a"} 1`,
		`tentacles_last_job_peak_memory_bytes{pool="org-a"} 5.36870912e+08`,
		`tentacles_slot_start_seconds_count{pool="org-b"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing sample %q:\n%s", want, body)
		}
	}
	first := strings.Index(body, `tentacles_desired_runners{pool="org-a"}`)
	second := strings.Index(body, `tentacles_desired_runners{pool="org-b"}`)
	if first < 0 || second < 0 || first > second {
		t.Errorf("pool samples are not sorted:\n%s", body)
	}
}

func TestRegisteredPoolExposesZeroSeries(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterPool("org-a")
	body := registry.render()
	for _, want := range []string{
		`tentacles_desired_runners{pool="org-a"} 0`,
		`tentacles_jobs_started_total{pool="org-a"} 0`,
		`tentacles_listener_errors_total{pool="org-a"} 0`,
		`tentacles_host_max_runners 0`,
		`tentacles_job_cpu_seconds_count{pool="org-a"} 0`,
		`tentacles_slot_start_seconds_count{pool="org-a"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing initial sample %q:\n%s", want, body)
		}
	}
}

func TestActualRunnersStateOverwrite(t *testing.T) {
	registry := NewRegistry()
	registry.SetActual("org-a", "idle", 1)
	registry.SetActual("org-a", "idle", 0)
	registry.SetActual("org-a", "busy", 3)
	body := registry.render()
	if !strings.Contains(body, `tentacles_actual_runners{pool="org-a",state="busy"} 3`) {
		t.Errorf("busy state missing:\n%s", body)
	}
	if !strings.Contains(body, `tentacles_actual_runners{pool="org-a",state="idle"} 0`) {
		t.Errorf("idle state missing or stale:\n%s", body)
	}
	if strings.Count(body, "tentacles_actual_runners{") != 2 {
		t.Errorf("want exactly two actual-runners samples:\n%s", body)
	}
}

func TestHistogramBuckets(t *testing.T) {
	registry := NewRegistry()
	registry.ObserveSlotStart("org-a", 2)
	registry.ObserveSlotStart("org-a", 0.03)
	registry.ObserveSlotStart("org-a", 61)
	body := registry.render()
	for _, want := range []string{
		`tentacles_slot_start_seconds_bucket{pool="org-a",le="0.05"} 1`,
		`tentacles_slot_start_seconds_bucket{pool="org-a",le="2.5"} 2`,
		`tentacles_slot_start_seconds_bucket{pool="org-a",le="+Inf"} 3`,
		`tentacles_slot_start_seconds_sum{pool="org-a"} 63.03`,
		`tentacles_slot_start_seconds_count{pool="org-a"} 3`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing histogram sample %q:\n%s", want, body)
		}
	}
}

func TestConcurrentIncrements(t *testing.T) {
	registry := NewRegistry()
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				registry.IncJobsStarted("org-a")
				registry.IncJobsCompleted("org-a", "success")
			}
		}()
	}
	workers.Wait()

	body := registry.render()
	if !strings.Contains(body, `tentacles_jobs_started_total{pool="org-a"} 1000`) {
		t.Errorf("jobs-started counter lost increments:\n%s", body)
	}
	if !strings.Contains(body, `tentacles_jobs_completed_total{pool="org-a",result="success"} 1000`) {
		t.Errorf("jobs-completed counter lost increments:\n%s", body)
	}
}

func TestHandlerServesTextFormat(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterPool("org-a")
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got, want := response.Body.String(), registry.render(); got != want {
		t.Errorf("handler body differs from render():\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\n"); got != `a\"b\\c\n` {
		t.Errorf("escapeLabel = %q", got)
	}
}
