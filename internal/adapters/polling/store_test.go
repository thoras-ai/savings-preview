package polling

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func webRequests(t *testing.T, dir string) []int64 {
	t.Helper()
	var reqs []int64
	if err := scan(dir, math.MaxInt64, nil, func(r *record) error {
		for _, c := range r.Containers {
			if c.Key == web {
				reqs = append(reqs, c.CPURequest)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return reqs
}

func withWebRequest(cpu string) Reading {
	c := testCluster(true)
	for i := 0; i < 2; i++ {
		c.Pods[i].Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = rl("cpu", cpu)[corev1.ResourceCPU]
	}
	return Reduce(c)
}

func openTest(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := OpenStore(dir, Meta{Cluster: "c", Step: 60, RollupStep: 300, Origin: 1_790_000_000})
	if err != nil {
		t.Fatal(err)
	}
	st.sync = false
	return st
}

// A reading torn after its level frame must not leave that level behind for
// the readings written after a restart.
func TestTornReadingLeavesNoLevels(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	st := openTest(t, dir)
	if err := st.Append(fromReading(1_790_000_000, 60, withWebRequest("100m"))); err != nil {
		t.Fatal(err)
	}
	torn := st.enc.encode(fromReading(1_790_000_060, 60, withWebRequest("200m")))
	st.cur.Write(torn[:len(torn)-3])
	st.Close()

	st = openTest(t, dir)
	if err := st.Append(fromReading(1_790_000_120, 60, withWebRequest("100m"))); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := webRequests(t, dir); len(got) != 2 || got[0] != 100e6 || got[1] != 100e6 {
		t.Errorf("requests after a torn reading: %v, want [100000000 100000000]", got)
	}
}

// A crash between writing a rollup and removing its raw segment leaves both;
// the rollup is read and the raw segment is removed.
func TestRollupAndRawBothPresent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	st := openTest(t, dir)
	for i := int64(0); i < 70; i++ {
		st.Append(fromReading(1_790_000_000+i*60, 60, withWebRequest("100m")))
	}
	raw := filepath.Join(dir, rawName(1_790_000_000))
	saved, _ := os.ReadFile(raw)
	if err := st.Compact(1_790_003_600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(raw, saved, 0o644)
	segs, _ := segments(dir)
	if segs[0].raw || !segs[0].stale {
		t.Fatalf("segment %+v: want the rollup, marked stale", segs[0])
	}
	if got := webRequests(t, dir); len(got) != 12+10 {
		t.Errorf("%d records, want 12 rolled steps and 10 raw readings", len(got))
	}
	if err := st.Compact(1_790_003_600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Errorf("stale raw segment not removed by the next Compact")
	}
	st.Close()

	os.WriteFile(raw, saved, 0o644)
	st = openTest(t, dir)
	st.Close()
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Errorf("stale raw segment not removed on open")
	}
}

// An export that listed a raw segment before it was rolled up reads the rollup.
func TestOpenSegmentFallsBackToRollup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	st := openTest(t, dir)
	for i := int64(0); i < 70; i++ {
		st.Append(fromReading(1_790_000_000+i*60, 60, withWebRequest("100m")))
	}
	listed, _ := segments(dir)
	if !listed[0].raw {
		t.Fatal("first segment should be raw before Compact")
	}
	if err := st.Compact(1_790_003_600); err != nil {
		t.Fatal(err)
	}
	st.Close()
	rc, err := openSegment(dir, listed[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	rec, err := newDecoder(rc).next()
	if err != nil || rec.Step != 300 || !rec.HasMax {
		t.Errorf("first record %+v, %v: want a rolled-up step", rec, err)
	}
}

func TestProgressCountsCoverageNotGaps(t *testing.T) {
	m := Meta{Step: 60, RollupStep: 300}
	var s Summary
	r := withWebRequest("100m")
	for i := int64(0); i < 1440; i++ {
		s.add(fromReading(1_790_000_000+i*60, 60, r))
	}
	s.add(fromReading(1_790_000_000+8*86400, 60, r))
	p := s.Progress(m, 48*time.Hour)
	for _, want := range []string{"covering 1d0h1m", "over 8d0h1m with 7d0h0m of gaps", "Tier 2 needs 7 days: 5d23h59m more"} {
		if !strings.Contains(p, want) {
			t.Errorf("progress %q lacks %q", p, want)
		}
	}
}
