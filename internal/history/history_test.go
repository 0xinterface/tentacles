package history

import (
	"math"
	"os"
	"path/filepath"
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
