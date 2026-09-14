// Package history keeps per-workflow resource usage statistics. Every
// finished job appends a Record; the store maintains exponentially
// weighted moving averages per workflow ref and globally, so the
// admission gate can predict what a queued job will cost before
// starting a slot for it.
//
// Records persist as JSON lines (one object per finished job). The file
// is replayed on open and rotated once it grows past RotateLines lines,
// so long-running daemons do not accumulate an unbounded log.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// EWMAAlpha is the smoothing factor for the moving averages: 0.25
// weights the last ~4 jobs of a workflow roughly equally.
const EWMAAlpha = 0.25

// Record is one finished job's measured resource usage.
type Record struct {
	Pool             string    `json:"pool"`
	Slot             string    `json:"slot"`
	WorkflowRef      string    `json:"workflow_ref"`
	RunID            int64     `json:"run_id,omitempty"`
	CPUSeconds       float64   `json:"cpu_seconds"`
	PeakMemBytes     uint64    `json:"peak_mem_bytes"`
	WallSeconds      float64   `json:"wall_seconds"`
	QueueWaitSeconds float64   `json:"queue_wait_seconds,omitempty"`
	Sampled          bool      `json:"sampled"`
	At               time.Time `json:"at"`
}

// Stats is the exponentially weighted summary for one workflow ref (or
// the global aggregate). CPUSeconds and PeakMemBytes only incorporate
// sampled records; WallSeconds incorporates every record.
type Stats struct {
	Count        int64
	CPUSeconds   float64
	PeakMemBytes uint64
	WallSeconds  float64
	sampledCount int64
}

// Store accumulates per-pool, per-ref usage history, persisted as JSON lines.
// All methods are safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string

	refs   map[historyKey]*Stats
	pools  map[string]*Stats
	global Stats

	// RotateLines caps the history file: once exceeded, the next Append
	// rewrites the newest lines. Zero disables rotation. Exposed for
	// tests; production uses defaultRotateLines.
	RotateLines int
}

type historyKey struct {
	pool string
	ref  string
}

const defaultRotateLines = 5000
const replayWindow = 5000 // records replayed from the tail on open

// Open loads (or creates) the history file at path and replays its
// records into the aggregate stats. A missing file starts empty.
func Open(path string) (*Store, error) {
	s := &Store{
		path:        path,
		refs:        make(map[historyKey]*Stats),
		pools:       make(map[string]*Stats),
		RotateLines: defaultRotateLines,
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	defer f.Close()

	// Replay only the newest window: read backwards in blocks.
	records, err := replayTail(f, replayWindow)
	if err != nil {
		return nil, fmt.Errorf("history: replay %s: %w", path, err)
	}
	for i := range records {
		s.absorb(records[i])
	}
	return s, nil
}

// Append records one finished job: it updates the aggregate stats and
// persists the record.
func (s *Store) Append(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("history: encode record: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("history: open %s: %w", s.path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("history: append %s: %w", s.path, err)
	}
	s.absorb(r)
	return nil
}

// Ref returns the stats for one workflow ref in a pool.
func (s *Store) Ref(pool, ref string) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stats := s.refs[historyKey{pool: pool, ref: ref}]; stats != nil {
		return *stats
	}
	return Stats{}
}

// Pool returns aggregate stats for one configured pool.
func (s *Store) Pool(pool string) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stats := s.pools[pool]; stats != nil {
		return *stats
	}
	return Stats{}
}

// Global returns stats across all pools and workflow refs.
func (s *Store) Global() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.global
}

// PredictCPU estimates CPU seconds from the workflow, then pool, then host
// history. The bool reports whether sampled history exists at any level.
func (s *Store) PredictCPU(pool, ref string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stats := s.refs[historyKey{pool: pool, ref: ref}]; stats != nil && stats.sampledCount > 0 {
		return stats.CPUSeconds, true
	}
	if stats := s.pools[pool]; stats != nil && stats.sampledCount > 0 {
		return stats.CPUSeconds, true
	}
	if s.global.sampledCount > 0 {
		return s.global.CPUSeconds, true
	}
	return 0, false
}

// PredictMem estimates peak memory using the same fallback as PredictCPU.
func (s *Store) PredictMem(pool, ref string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stats := s.refs[historyKey{pool: pool, ref: ref}]; stats != nil && stats.sampledCount > 0 {
		return stats.PeakMemBytes, true
	}
	if stats := s.pools[pool]; stats != nil && stats.sampledCount > 0 {
		return stats.PeakMemBytes, true
	}
	if s.global.sampledCount > 0 {
		return s.global.PeakMemBytes, true
	}
	return 0, false
}

// absorb folds one record into its workflow, pool, and host aggregates.
// Callers hold s.mu.
func (s *Store) absorb(record Record) {
	key := historyKey{pool: record.Pool, ref: record.WorkflowRef}
	refStats := s.refs[key]
	if refStats == nil {
		refStats = &Stats{}
		s.refs[key] = refStats
	}
	poolStats := s.pools[record.Pool]
	if poolStats == nil {
		poolStats = &Stats{}
		s.pools[record.Pool] = poolStats
	}
	blend(refStats, record)
	blend(poolStats, record)
	blend(&s.global, record)
}

// blend folds one record into one aggregate: sampled records feed the
// CPU and memory averages; every record feeds the wall-time average.
// The first record seeds the averages instead of blending toward zero.
func blend(st *Stats, r Record) {
	if r.Sampled {
		if st.sampledCount == 0 {
			st.CPUSeconds = r.CPUSeconds
			st.PeakMemBytes = r.PeakMemBytes
		} else {
			st.CPUSeconds = EWMAAlpha*r.CPUSeconds + (1-EWMAAlpha)*st.CPUSeconds
			st.PeakMemBytes = uint64(EWMAAlpha*float64(r.PeakMemBytes) + (1-EWMAAlpha)*float64(st.PeakMemBytes) + 0.5)
		}
		st.sampledCount++
	}
	if st.Count == 0 {
		st.WallSeconds = r.WallSeconds
	} else {
		st.WallSeconds = EWMAAlpha*r.WallSeconds + (1-EWMAAlpha)*st.WallSeconds
	}
	st.Count++
}

// rotateLocked rewrites the file with its newest half once it grows
// past RotateLines lines. Callers hold s.mu.
func (s *Store) rotateLocked() error {
	limit := s.RotateLines
	if limit <= 0 {
		return nil
	}
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("history: stat %s: %w", s.path, err)
	}
	// Cheap pre-check: a record is at least ~250 bytes, so a file this
	// small cannot hold `limit` lines. The exact line count is checked
	// while rewriting.
	if fi.Size() < int64(limit)*250 {
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("history: open %s: %w", s.path, err)
	}
	defer f.Close()
	records, err := replayTail(f, limit/2)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("history: create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(out)
	for i := range records {
		line, err := json.Marshal(records[i])
		if err == nil {
			w.Write(line)
			w.WriteByte('\n')
		}
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return fmt.Errorf("history: rotate %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("history: close %s: %w", tmp, err)
	}
	return os.Rename(tmp, s.path)
}

// replayTail decodes the newest n records from r. The whole file is
// decoded: records have no size bound (long workflow refs), so a byte
// window could start mid-line and a single torn fragment after a crash
// must not discard the records around it. Malformed lines are skipped.
func replayTail(r *os.File, n int) ([]Record, error) {
	if _, err := r.Seek(0, 0); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // one long line is fine
	var out []Record
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // tolerate a torn tail from a crash mid-append
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// PredictCores estimates average CPU rate using the same workflow, pool,
// then host fallback as the other predictions.
func (s *Store) PredictCores(pool, ref string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stats := s.refs[historyKey{pool: pool, ref: ref}]; usableRate(stats) {
		return stats.CPUSeconds / stats.WallSeconds, true
	}
	if stats := s.pools[pool]; usableRate(stats) {
		return stats.CPUSeconds / stats.WallSeconds, true
	}
	if usableRate(&s.global) {
		return s.global.CPUSeconds / s.global.WallSeconds, true
	}
	return 0, false
}

func usableRate(stats *Stats) bool {
	return stats != nil && stats.sampledCount > 0 && stats.WallSeconds > 0
}
