// Package metrics implements the tentacles Prometheus metrics registry.
//
// The exposition is hand-rolled on purpose: the daemon avoids third-party
// metrics clients, so the registry owns a small Prometheus text-format
// renderer with one bounded pool label from configuration.
package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var slotStartBuckets = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

var (
	jobCPUBuckets  = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	jobWallBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}
)

// Registry is the daemon-wide metrics registry.
type Registry struct {
	mu             sync.Mutex
	hostMaxRunners int
	pools          map[string]*poolMetrics
}

type poolMetrics struct {
	desired        int
	actual         map[string]int
	jobsStarted    uint64
	jobsCompleted  map[string]uint64
	acquireFails   uint64
	listenerErrs   uint64
	lastMessageID  int64
	admissionHolds uint64
	jobCPU         *histogram
	jobWall        *histogram
	slotStartCount uint64
	slotStartSum   float64
	slotStarts     []uint64
	lastJobPeakMem uint64
}

type histogram struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{
		buckets: buckets,
		counts:  make([]uint64, len(buckets)),
	}
}

func newPoolMetrics() *poolMetrics {
	return &poolMetrics{
		actual:        make(map[string]int),
		jobsCompleted: make(map[string]uint64),
		jobCPU:        newHistogram(jobCPUBuckets),
		jobWall:       newHistogram(jobWallBuckets),
		slotStarts:    make([]uint64, len(slotStartBuckets)+1),
	}
}

func (h *histogram) observe(value float64) {
	h.count++
	h.sum += value
	for i, upper := range h.buckets {
		if value <= upper {
			h.counts[i]++
		}
	}
}

func (h *histogram) lines(name, pool string) []string {
	label := `pool="` + escapeLabel(pool) + `"`
	lines := make([]string, 0, len(h.buckets)+3)
	for i, upper := range h.buckets {
		lines = append(
			lines,
			name+`_bucket{`+label+`,le="`+formatFloat(upper)+`"} `+
				formatFloat(float64(h.counts[i])),
		)
	}
	lines = append(
		lines,
		name+`_bucket{`+label+`,le="+Inf"} `+formatFloat(float64(h.count)),
		name+"_sum{"+label+"} "+formatFloat(h.sum),
		name+"_count{"+label+"} "+formatFloat(float64(h.count)),
	)
	return lines
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{pools: make(map[string]*poolMetrics)}
}

// RegisterPool creates the fixed zero-valued series for one configured pool.
func (r *Registry) RegisterPool(pool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool)
}

// SetHostMaxRunners records the global scheduler ceiling.
func (r *Registry) SetHostMaxRunners(value int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hostMaxRunners = value
}

func (r *Registry) poolLocked(pool string) *poolMetrics {
	stats := r.pools[pool]
	if stats == nil {
		stats = newPoolMetrics()
		r.pools[pool] = stats
	}
	return stats
}

func (r *Registry) SetDesired(pool string, value int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).desired = value
}

func (r *Registry) SetActual(pool, state string, value int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).actual[state] = value
}

func (r *Registry) IncJobsStarted(pool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).jobsStarted++
}

func (r *Registry) IncJobsCompleted(pool, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).jobsCompleted[result]++
}

func (r *Registry) IncAcquireFailures(pool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).acquireFails++
}

func (r *Registry) IncListenerErrors(pool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).listenerErrs++
}

func (r *Registry) SetLastMessageID(pool string, id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).lastMessageID = id
}

func (r *Registry) ObserveSlotStart(pool string, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.poolLocked(pool)
	stats.slotStartCount++
	stats.slotStartSum += seconds
	for i, upper := range slotStartBuckets {
		if seconds <= upper {
			stats.slotStarts[i]++
		}
	}
	stats.slotStarts[len(slotStartBuckets)]++
}

func (r *Registry) IncAdmissionHolds(pool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).admissionHolds++
}

func (r *Registry) ObserveJobCPU(pool string, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).jobCPU.observe(seconds)
}

func (r *Registry) ObserveJobWall(pool string, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).jobWall.observe(seconds)
}

func (r *Registry) SetJobPeakMem(pool string, bytes uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolLocked(pool).lastJobPeakMem = bytes
}

// Handler serves the registry in Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.render()))
	})
}

type family struct {
	name  string
	help  string
	typ   string
	lines []string
}

func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	pools := r.poolNames()

	families := []family{
		{
			name: "tentacles_acquire_failures_total",
			help: "Total number of times a runner failed to acquire its assigned job.",
			typ:  "counter",
			lines: r.scalarLines("tentacles_acquire_failures_total", pools, func(stats *poolMetrics) float64 {
				return float64(stats.acquireFails)
			}),
		},
		{
			name:  "tentacles_actual_runners",
			help:  "Current number of runner slots by pool and lifecycle state.",
			typ:   "gauge",
			lines: r.actualLines(pools),
		},
		{
			name: "tentacles_desired_runners",
			help: "Current desired number of runner slots by pool.",
			typ:  "gauge",
			lines: r.scalarLines("tentacles_desired_runners", pools, func(stats *poolMetrics) float64 {
				return float64(stats.desired)
			}),
		},
		{
			name: "tentacles_host_max_runners",
			help: "Configured maximum number of runners on this host.",
			typ:  "gauge",
			lines: []string{
				"tentacles_host_max_runners " + formatFloat(float64(r.hostMaxRunners)),
			},
		},
		{
			name:  "tentacles_jobs_completed_total",
			help:  "Total number of completed jobs by pool and reported result.",
			typ:   "counter",
			lines: r.jobsCompletedLines(pools),
		},
		{
			name: "tentacles_jobs_started_total",
			help: "Total number of started jobs by pool.",
			typ:  "counter",
			lines: r.scalarLines("tentacles_jobs_started_total", pools, func(stats *poolMetrics) float64 {
				return float64(stats.jobsStarted)
			}),
		},
		{
			name: "tentacles_last_message_id",
			help: "ID of the last processed scale-set message by pool.",
			typ:  "gauge",
			lines: r.scalarLines("tentacles_last_message_id", pools, func(stats *poolMetrics) float64 {
				return float64(stats.lastMessageID)
			}),
		},
		{
			name: "tentacles_listener_errors_total",
			help: "Total number of scale-set listener loop errors by pool.",
			typ:  "counter",
			lines: r.scalarLines("tentacles_listener_errors_total", pools, func(stats *poolMetrics) float64 {
				return float64(stats.listenerErrs)
			}),
		},
		{
			name: "tentacles_admission_holds_total",
			help: "Total number of slot starts held by the admission gate by pool.",
			typ:  "counter",
			lines: r.scalarLines("tentacles_admission_holds_total", pools, func(stats *poolMetrics) float64 {
				return float64(stats.admissionHolds)
			}),
		},
		{
			name: "tentacles_job_cpu_seconds",
			help: "CPU seconds consumed by finished jobs.",
			typ:  "histogram",
			lines: r.histogramLines("tentacles_job_cpu_seconds", pools, func(stats *poolMetrics) *histogram {
				return stats.jobCPU
			}),
		},
		{
			name: "tentacles_job_wall_seconds",
			help: "Wall-clock seconds of finished jobs.",
			typ:  "histogram",
			lines: r.histogramLines("tentacles_job_wall_seconds", pools, func(stats *poolMetrics) *histogram {
				return stats.jobWall
			}),
		},
		{
			name: "tentacles_last_job_peak_memory_bytes",
			help: "Peak memory of the most recent finished job by pool.",
			typ:  "gauge",
			lines: r.scalarLines("tentacles_last_job_peak_memory_bytes", pools, func(stats *poolMetrics) float64 {
				return float64(stats.lastJobPeakMem)
			}),
		},
		{
			name:  "tentacles_slot_start_seconds",
			help:  "Provision time of successful slot starts.",
			typ:   "histogram",
			lines: r.slotStartLines(pools),
		},
	}
	sort.Slice(families, func(i, j int) bool { return families[i].name < families[j].name })

	var builder strings.Builder
	for _, metricFamily := range families {
		if len(metricFamily.lines) == 0 {
			continue
		}
		builder.WriteString("# HELP ")
		builder.WriteString(metricFamily.name)
		builder.WriteByte(' ')
		builder.WriteString(metricFamily.help)
		builder.WriteByte('\n')
		builder.WriteString("# TYPE ")
		builder.WriteString(metricFamily.name)
		builder.WriteByte(' ')
		builder.WriteString(metricFamily.typ)
		builder.WriteByte('\n')
		for _, line := range metricFamily.lines {
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func (r *Registry) poolNames() []string {
	names := make([]string, 0, len(r.pools))
	for name := range r.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) scalarLines(
	name string,
	pools []string,
	value func(*poolMetrics) float64,
) []string {
	lines := make([]string, 0, len(pools))
	for _, pool := range pools {
		lines = append(
			lines,
			name+`{pool="`+escapeLabel(pool)+`"} `+formatFloat(value(r.pools[pool])),
		)
	}
	return lines
}

func (r *Registry) actualLines(pools []string) []string {
	lines := make([]string, 0, len(pools)*6)
	for _, pool := range pools {
		stats := r.pools[pool]
		states := make([]string, 0, len(stats.actual))
		for state := range stats.actual {
			states = append(states, state)
		}
		sort.Strings(states)
		for _, state := range states {
			lines = append(
				lines,
				`tentacles_actual_runners{pool="`+escapeLabel(pool)+`",state="`+
					escapeLabel(state)+`"} `+formatFloat(float64(stats.actual[state])),
			)
		}
	}
	return lines
}

func (r *Registry) jobsCompletedLines(pools []string) []string {
	lines := []string{}
	for _, pool := range pools {
		stats := r.pools[pool]
		results := make([]string, 0, len(stats.jobsCompleted))
		for result := range stats.jobsCompleted {
			results = append(results, result)
		}
		sort.Strings(results)
		for _, result := range results {
			lines = append(
				lines,
				`tentacles_jobs_completed_total{pool="`+escapeLabel(pool)+`",result="`+
					escapeLabel(result)+`"} `+formatFloat(float64(stats.jobsCompleted[result])),
			)
		}
	}
	return lines
}

func (r *Registry) histogramLines(
	name string,
	pools []string,
	selectHistogram func(*poolMetrics) *histogram,
) []string {
	lines := []string{}
	for _, pool := range pools {
		lines = append(lines, selectHistogram(r.pools[pool]).lines(name, pool)...)
	}
	return lines
}

func (r *Registry) slotStartLines(pools []string) []string {
	lines := make([]string, 0, len(pools)*(len(slotStartBuckets)+3))
	for _, pool := range pools {
		stats := r.pools[pool]
		label := `pool="` + escapeLabel(pool) + `"`
		for i, upper := range slotStartBuckets {
			lines = append(
				lines,
				`tentacles_slot_start_seconds_bucket{`+label+`,le="`+
					formatFloat(upper)+`"} `+formatFloat(float64(stats.slotStarts[i])),
			)
		}
		lines = append(
			lines,
			`tentacles_slot_start_seconds_bucket{`+label+`,le="+Inf"} `+
				formatFloat(float64(stats.slotStarts[len(slotStartBuckets)])),
			"tentacles_slot_start_seconds_sum{"+label+"} "+formatFloat(stats.slotStartSum),
			"tentacles_slot_start_seconds_count{"+label+"} "+
				formatFloat(float64(stats.slotStartCount)),
		)
	}
	return lines
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}
