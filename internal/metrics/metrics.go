// Package metrics implements the tentacles Prometheus metrics registry.
//
// The exposition is hand-rolled on purpose: the daemon avoids third-party
// metrics clients, so the registry owns a small Prometheus text-format
// renderer (version 0.0.4) with a fixed, documented metric set. All methods
// are safe for concurrent use; the rendered output is sorted and stable so
// tests (and humans) can diff it exactly.
package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// slotStartBuckets are the le (less-than-or-equal) boundaries of the
// tentacles_slot_start_seconds histogram.
var slotStartBuckets = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// jobCPUBuckets and jobWallBuckets bound typical per-job CPU time and
// wall-clock spans (seconds).
var (
	jobCPUBuckets  = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	jobWallBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}
)

// Registry is the daemon-wide metrics registry.
type Registry struct {
	mu sync.Mutex

	desired       int
	actual        map[string]int
	jobsStarted   uint64
	jobsCompleted map[string]uint64
	acquireFails  uint64
	listenerErrs  uint64
	lastMessageID int64

	admissionHolds uint64

	jobCPU  *histogram
	jobWall *histogram

	slotStartCount uint64
	slotStartSum   float64
	// slotStartBuckets holds one count per slotStartBuckets entry, plus a
	// final +Inf bucket.
	slotStartBuckets []uint64
	lastJobPeakMem   uint64
}

// histogram is a fixed-bucket cumulative histogram.
type histogram struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{buckets: buckets, counts: make([]uint64, len(buckets))}
}

func (h *histogram) observe(v float64) {
	h.count++
	h.sum += v
	for i, le := range h.buckets {
		if v <= le {
			h.counts[i]++
		}
	}
}

func (h *histogram) lines(name string) []string {
	lines := make([]string, 0, len(h.buckets)+3)
	for i, le := range h.buckets {
		lines = append(lines, name+"_bucket{le=\""+formatFloat(le)+"\"} "+formatFloat(float64(h.counts[i])))
	}
	lines = append(lines, name+"_bucket{le=\"+Inf\"} "+formatFloat(float64(h.count)))
	lines = append(lines, name+"_sum "+formatFloat(h.sum))
	lines = append(lines, name+"_count "+formatFloat(float64(h.count)))
	return lines
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		actual:           make(map[string]int),
		jobsCompleted:    make(map[string]uint64),
		slotStartBuckets: make([]uint64, len(slotStartBuckets)+1),
		jobCPU:           newHistogram(jobCPUBuckets),
		jobWall:          newHistogram(jobWallBuckets),
	}
}

// SetDesired records the last desired runner count.
func (r *Registry) SetDesired(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.desired = n
}

// SetActual records how many runner slots are in the given state. A state
// shows up in the exposition once it has been set here.
func (r *Registry) SetActual(state string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actual[state] = n
}

// IncJobsStarted counts one started job.
func (r *Registry) IncJobsStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobsStarted++
}

// IncJobsCompleted counts one completed job under the given result label.
// Unknown result labels are allowed and produce their own series.
func (r *Registry) IncJobsCompleted(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobsCompleted[result]++
}

// IncAcquireFailures counts one runner acquire failure.
func (r *Registry) IncAcquireFailures() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acquireFails++
}

// IncListenerErrors counts one listener loop error.
func (r *Registry) IncListenerErrors() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listenerErrs++
}

// SetLastMessageID records the ID of the last processed scale-set message.
func (r *Registry) SetLastMessageID(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastMessageID = id
}

// ObserveSlotStart records how long a slot took to start, in seconds.
func (r *Registry) ObserveSlotStart(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slotStartCount++
	r.slotStartSum += seconds
	for i, le := range slotStartBuckets {
		if seconds <= le {
			r.slotStartBuckets[i]++
		}
	}
	r.slotStartBuckets[len(slotStartBuckets)]++ // +Inf
}

// IncAdmissionHolds counts one slot start held back by the admission gate.
func (r *Registry) IncAdmissionHolds() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.admissionHolds++
}

// ObserveJobCPU records the measured CPU seconds of one finished job.
func (r *Registry) ObserveJobCPU(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobCPU.observe(seconds)
}

// ObserveJobWall records the wall-clock seconds of one finished job.
func (r *Registry) ObserveJobWall(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobWall.observe(seconds)
}

// SetJobPeakMem records the peak memory of the most recent finished job.
func (r *Registry) SetJobPeakMem(b uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastJobPeakMem = b
}

// Handler returns an HTTP handler serving the registry in Prometheus text
// exposition format (version 0.0.4).
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.render()))
	})
}

// family is one Prometheus metric family in exposition order.
type family struct {
	name  string
	help  string
	typ   string
	lines []string
}

// render produces the full exposition. Families are emitted in sorted name
// order; samples within a family are sorted by their label values.
func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	families := []family{
		{
			name: "tentacles_acquire_failures_total",
			help: "Total number of times a runner failed to acquire its assigned job.",
			typ:  "counter",
			lines: []string{
				"tentacles_acquire_failures_total " + formatFloat(float64(r.acquireFails)),
			},
		},
		{
			name:  "tentacles_actual_runners",
			help:  "Current number of runner slots by lifecycle state.",
			typ:   "gauge",
			lines: r.actualLines(),
		},
		{
			name: "tentacles_desired_runners",
			help: "Current desired number of runner slots.",
			typ:  "gauge",
			lines: []string{
				"tentacles_desired_runners " + formatFloat(float64(r.desired)),
			},
		},
		{
			name:  "tentacles_jobs_completed_total",
			help:  "Total number of completed jobs, by result.",
			typ:   "counter",
			lines: r.jobsCompletedLines(),
		},
		{
			name: "tentacles_jobs_started_total",
			help: "Total number of started jobs.",
			typ:  "counter",
			lines: []string{
				"tentacles_jobs_started_total " + formatFloat(float64(r.jobsStarted)),
			},
		},
		{
			name: "tentacles_last_message_id",
			help: "ID of the last processed scale-set message.",
			typ:  "gauge",
			lines: []string{
				"tentacles_last_message_id " + formatFloat(float64(r.lastMessageID)),
			},
		},
		{
			name: "tentacles_listener_errors_total",
			help: "Total number of scale-set listener loop errors.",
			typ:  "counter",
			lines: []string{
				"tentacles_listener_errors_total " + formatFloat(float64(r.listenerErrs)),
			},
		},
		{
			name: "tentacles_admission_holds_total",
			help: "Total number of slot starts held back by the admission gate.",
			typ:  "counter",
			lines: []string{
				"tentacles_admission_holds_total " + formatFloat(float64(r.admissionHolds)),
			},
		},
		{
			name:  "tentacles_job_cpu_seconds",
			help:  "CPU seconds consumed by finished jobs.",
			typ:   "histogram",
			lines: r.jobCPU.lines("tentacles_job_cpu_seconds"),
		},
		{
			name:  "tentacles_job_wall_seconds",
			help:  "Wall-clock seconds of finished jobs.",
			typ:   "histogram",
			lines: r.jobWall.lines("tentacles_job_wall_seconds"),
		},
		{
			name: "tentacles_last_job_peak_memory_bytes",
			help: "Peak memory of the most recent finished job.",
			typ:  "gauge",
			lines: []string{
				"tentacles_last_job_peak_memory_bytes " + formatFloat(float64(r.lastJobPeakMem)),
			},
		},
		{
			name:  "tentacles_slot_start_seconds",
			help:  "Time to start a runner slot.",
			typ:   "histogram",
			lines: r.slotStartLines(),
		},
	}

	sort.Slice(families, func(i, j int) bool { return families[i].name < families[j].name })

	var b strings.Builder
	for _, f := range families {
		if len(f.lines) == 0 {
			continue
		}
		b.WriteString("# HELP ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(f.help)
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(f.typ)
		b.WriteByte('\n')
		for _, l := range f.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// actualLines returns one sample per state that has been set, sorted by
// state name. No states set means no samples and no family header.
func (r *Registry) actualLines() []string {
	if len(r.actual) == 0 {
		return nil
	}
	states := make([]string, 0, len(r.actual))
	for s := range r.actual {
		states = append(states, s)
	}
	sort.Strings(states)
	lines := make([]string, 0, len(states))
	for _, s := range states {
		lines = append(lines, "tentacles_actual_runners{state=\""+escapeLabel(s)+"\"} "+formatFloat(float64(r.actual[s])))
	}
	return lines
}

// jobsCompletedLines returns one sample per result that has been seen,
// sorted by result name.
func (r *Registry) jobsCompletedLines() []string {
	if len(r.jobsCompleted) == 0 {
		return nil
	}
	results := make([]string, 0, len(r.jobsCompleted))
	for res := range r.jobsCompleted {
		results = append(results, res)
	}
	sort.Strings(results)
	lines := make([]string, 0, len(results))
	for _, res := range results {
		lines = append(lines, "tentacles_jobs_completed_total{result=\""+escapeLabel(res)+"\"} "+formatFloat(float64(r.jobsCompleted[res])))
	}
	return lines
}

// slotStartLines returns the histogram bucket lines, then _sum and _count.
func (r *Registry) slotStartLines() []string {
	lines := make([]string, 0, len(slotStartBuckets)+3)
	for i, le := range slotStartBuckets {
		lines = append(lines, "tentacles_slot_start_seconds_bucket{le=\""+formatFloat(le)+"\"} "+formatFloat(float64(r.slotStartBuckets[i])))
	}
	lines = append(lines, "tentacles_slot_start_seconds_bucket{le=\"+Inf\"} "+formatFloat(float64(r.slotStartBuckets[len(slotStartBuckets)])))
	lines = append(lines, "tentacles_slot_start_seconds_sum "+formatFloat(r.slotStartSum))
	lines = append(lines, "tentacles_slot_start_seconds_count "+formatFloat(float64(r.slotStartCount)))
	return lines
}

// formatFloat renders v as the shortest decimal that round-trips, which is
// exactly how Prometheus itself formats sample values.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabel escapes a label value per the Prometheus text format.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
