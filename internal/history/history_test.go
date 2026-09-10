package history

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rec(ref string, cpu float64, mem uint64, wall float64, sampled bool) Record {
	return Record{
		Slot: "0001", WorkflowRef: ref, CPUSeconds: cpu,
		PeakMemBytes: mem, WallSeconds: wall, Sampled: sampled,
		At: time.Now(),
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.001 }

func TestStoreStatsAndEWMA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "o/r/w.yml@main"
	if st := s.Ref(ref); st.Count != 0 {
		t.Fatalf("unknown ref stats = %+v, want zero", st)
	}
	if err := s.Append(rec(ref, 10, 1<<28, 60, true)); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(rec(ref, 20, 3<<28, 90, true)); err != nil {
		t.Fatal(err)
	}
	st := s.Ref(ref)
	// EWMA alpha 0.25 weights the newest sample: after 10 then 20 the
	// average sits at 0.25*20 + 0.75*10.
	wantCPU := 0.25*20 + 0.75*10 // 12.5
	if !near(st.CPUSeconds, wantCPU) {
		t.Fatalf("cpu ewma = %v, want %v", st.CPUSeconds, wantCPU)
	}
	if st.PeakMemBytes != uint64(0.25*(3<<28)+0.75*(1<<28)) {
		t.Fatalf("mem ewma = %v", st.PeakMemBytes)
	}
	if !near(st.WallSeconds, 0.25*90+0.75*60) {
		t.Fatalf("wall ewma = %v", st.WallSeconds)
	}
	// Global aggregates every ref.
	g := s.Global()
	if g.Count != 2 || !near(g.CPUSeconds, wantCPU) {
		t.Fatalf("global = %+v", g)
	}
}

func TestStoreUnsampledRecordsSkipCPUMem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	const ref = "o/r/w.yml@main"
	if err := s.Append(rec(ref, 0, 0, 45, false)); err != nil {
		t.Fatal(err)
	}
	st := s.Ref(ref)
	if st.Count != 1 {
		t.Fatalf("count = %d", st.Count)
	}
	if st.CPUSeconds != 0 || st.PeakMemBytes != 0 {
		t.Fatalf("unsampled record fed cpu/mem stats: %+v", st)
	}
	if !near(st.WallSeconds, 45) {
		t.Fatalf("wall ewma = %v, want 45", st.WallSeconds)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	const ref = "o/r/w.yml@main"
	if err := s.Append(rec(ref, 30, 2<<30, 120, true)); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Ref(ref)
	if st.Count != 1 || !near(st.CPUSeconds, 30) || st.PeakMemBytes != 2<<30 {
		t.Fatalf("reopened stats = %+v", st)
	}
}

func TestStoreRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	s.RotateLines = 4
	for i := range 10 {
		if err := s.Append(rec("o/r/w.yml@main", float64(i), 0, float64(i), true)); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() > 1<<20 {
		t.Fatalf("file not rotated: size=%v err=%v", fi, err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := s2.Global(); st.Count == 0 {
		t.Fatal("rotation lost all records")
	}
}

func TestPredictCoresFallsBackToGlobal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	// Only ref A has history.
	if err := s.Append(Record{WorkflowRef: "a", CPUSeconds: 20, WallSeconds: 10, Sampled: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cores, ok := s.PredictCores("a")
	if !ok || !near(cores, 2) {
		t.Fatalf("a cores = %v ok=%v, want 2", cores, ok)
	}
	// Unknown ref falls back to the global average.
	cores, ok = s.PredictCores("unknown")
	if !ok || !near(cores, 2) {
		t.Fatalf("fallback cores = %v ok=%v, want 2", cores, ok)
	}
	// No history at all: inert.
	empty, _ := Open(filepath.Join(t.TempDir(), "h.jsonl"))
	if _, ok := empty.PredictCores("a"); ok {
		t.Fatal("empty store predicted cores")
	}
}

// TestStoreReplayWithLongRefs: real workflow refs (owner/repo/
// .github/workflows/x.yml@refs/heads/feature/...) push records well
// past a few hundred bytes. Replay must not lose records for exceeding
// a byte-size guess, and rotation must never wipe the file empty.
func TestStoreReplayWithLongRefs(t *testing.T) {
	longRef := "o/r/.github/workflows/ci.yml@refs/heads/feature/" + strings.Repeat("x", 300)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	if err := s.Append(rec(longRef, 42, 7<<28, 88, true)); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Ref(longRef)
	if st.Count != 1 || !near(st.CPUSeconds, 42) {
		t.Fatalf("long-ref record lost on reopen: %+v", st)
	}

	// Rotation with long records keeps the newest records instead of
	// rewriting the file empty.
	s2.RotateLines = 2
	if err := s2.Append(rec(longRef, 50, 0, 90, true)); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := s3.Ref(longRef); st.Count == 0 {
		t.Fatal("rotation wiped long-ref history")
	}
}

// TestStoreReplayOversizedFile: once the file exceeds the replay window
// (5000 * 256B), a byte-window can start mid-line and the decoder must
// not lose every record to one torn fragment. Drives the window overflow
// with a raw oversized file (~1.4MB, long refs).
func TestStoreReplayOversizedFile(t *testing.T) {
	longRef := "o/r/.github/workflows/ci.yml@refs/heads/feature/" + strings.Repeat("x", 300)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	var b strings.Builder
	for range 3200 {
		b.WriteString(fmt.Sprintf(
			`{"slot":"0001","workflow_ref":%q,"cpu_seconds":7,"peak_mem_bytes":1000,"wall_seconds":20,"sampled":true,"at":"2026-01-01T00:00:00Z"}`+"\n", longRef))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1<<20 {
		t.Fatalf("fixture too small to overflow the window: %v %v", fi, err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Global(); st.Count < 3000 {
		t.Fatalf("replayed only %d of 3200 records", st.Count)
	}
}

// TestStoreRotateKeepsNewestWithLongRefs: rotation decodes the whole
// file, so it keeps the newest records even when every record exceeds
// the old 256-byte guess.
func TestStoreRotateKeepsNewestWithLongRefs(t *testing.T) {
	longRef := "o/r/.github/workflows/ci.yml@refs/heads/feature/" + strings.Repeat("x", 300)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path)
	s.RotateLines = 4
	for range 10 {
		if err := s.Append(rec(longRef, 1, 0, 5, true)); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := s2.Global(); st.Count < 2 {
		t.Fatalf("rotation lost records: kept %d", st.Count)
	}
}
