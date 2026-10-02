package polling

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The readings directory holds:
//
//	meta.json             cluster, step, rollup step and grid origin
//	lock                  held by the poller writing the directory
//	raw-<start>.bin       readings at the polling step, one file per segment
//	roll-<start>.bin.gz   the same segment rolled up to the rollup step
//
// Segments cover a fixed span starting at origin + k*span. Raw segments older
// than the retention are rolled up: the rollup is written to a temporary file
// and renamed, then the raw segment is removed. If both exist the rollup wins.

const metaVersion = 2

// Meta describes a readings directory.
type Meta struct {
	Version    int    `json:"version"`
	Cluster    string `json:"cluster"`
	Step       int    `json:"step"`
	RollupStep int    `json:"rollup_step"`
	Origin     int64  `json:"origin"`
}

// SegmentSeconds is the span of one segment: about an hour, and a whole number
// of rollup steps so that no rollup step straddles two segments.
func (m Meta) SegmentSeconds() int64 {
	rs := int64(m.RollupStep)
	return rs * ((3600 + rs - 1) / rs)
}

func (m Meta) segmentOf(t int64) int64 {
	n := m.SegmentSeconds()
	k := (t - m.Origin) / n
	if t < m.Origin && (t-m.Origin)%n != 0 {
		k--
	}
	return m.Origin + k*n
}

// rollupStepFor rounds want up to a multiple of step, and never below it.
func rollupStepFor(step int, want int) int {
	if want <= step {
		return step
	}
	return (want + step - 1) / step * step
}

func rawName(start int64) string  { return fmt.Sprintf("raw-%d.bin", start) }
func rollName(start int64) string { return fmt.Sprintf("roll-%d.bin.gz", start) }

// ErrLocked means another poller holds the readings directory.
var ErrLocked = errors.New("locked")

func readMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Version != metaVersion || m.Step <= 0 || m.RollupStep < m.Step {
		return m, fmt.Errorf("%s is not a readings directory of version %d", dir, metaVersion)
	}
	return m, nil
}

type segment struct {
	start int64
	raw   bool // false: rolled up
	stale bool // rolled up, but the raw file is still there
}

// segments lists the directory's segments in time order.
func segments(dir string) ([]segment, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	kinds := map[int64]bool{} // start -> raw only
	both := map[int64]bool{}
	for _, e := range ents {
		name := e.Name()
		var num string
		raw := false
		switch {
		case strings.HasPrefix(name, "raw-") && strings.HasSuffix(name, ".bin"):
			num, raw = strings.TrimSuffix(strings.TrimPrefix(name, "raw-"), ".bin"), true
		case strings.HasPrefix(name, "roll-") && strings.HasSuffix(name, ".bin.gz"):
			num = strings.TrimSuffix(strings.TrimPrefix(name, "roll-"), ".bin.gz")
		default:
			continue
		}
		start, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			continue
		}
		if was, ok := kinds[start]; ok {
			kinds[start] = was && raw
			both[start] = true
		} else {
			kinds[start] = raw
		}
	}
	out := make([]segment, 0, len(kinds))
	for s, raw := range kinds {
		out = append(out, segment{s, raw, both[s]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out, nil
}

// openSegment opens a segment for reading. A raw segment rolled up since it was
// listed is read from its rollup.
func openSegment(dir string, s segment) (io.ReadCloser, error) {
	if s.raw {
		f, err := os.Open(filepath.Join(dir, rawName(s.start)))
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	f, err := os.Open(filepath.Join(dir, rollName(s.start)))
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{z, f}, nil
}

// scan calls fn for every record with T <= until, in time order. With want,
// records hold only the entries whose key want accepts. It only reads, so it is
// safe while a poller appends: a frame still being written ends its segment.
func scan(dir string, until int64, want func(key string) bool, fn func(*record) error) error {
	segs, err := segments(dir)
	if err != nil {
		return err
	}
	for _, s := range segs {
		rc, err := openSegment(dir, s)
		if err != nil {
			return err
		}
		d := newDecoder(rc)
		d.want = want
		for {
			rec, err := d.next()
			if err != nil {
				break
			}
			if rec.T > until {
				break
			}
			if err := fn(rec); err != nil {
				rc.Close()
				return err
			}
		}
		rc.Close()
	}
	return nil
}

// Store appends readings to a readings directory it holds the lock on.
type Store struct {
	Dir  string
	Meta Meta
	lock io.Closer
	// sync makes every append durable before the next reading.
	sync bool

	cur      *os.File
	curStart int64
	enc      *encoder
}

// OpenStore locks dir, creating it with m when it is new. An existing
// directory must match m's cluster, step and rollup step; its origin is kept.
// Leftover temporary files are removed, raw segments that were already
// rolled up are deleted, and a torn tail is cut from every raw segment.
func OpenStore(dir string, m Meta) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lk, err := lockFile(filepath.Join(dir, "lock"))
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("another poller is writing %s; stop it or choose another -o", dir)
		}
		return nil, err
	}
	st := &Store{Dir: dir, lock: lk, sync: true}
	if err := st.prepare(m); err != nil {
		lk.Close()
		return nil, err
	}
	return st, nil
}

func (st *Store) prepare(m Meta) error {
	m.Version = metaVersion
	old, err := readMeta(st.Dir)
	switch {
	case err == nil:
		if old.Cluster != m.Cluster || old.Step != m.Step || old.RollupStep != m.RollupStep {
			return fmt.Errorf("%s holds readings of cluster %q every %ds rolled up to %ds; continue it with the same cluster, --interval and --rollup, or choose another -o",
				st.Dir, old.Cluster, old.Step, old.RollupStep)
		}
		st.Meta = old
	case os.IsNotExist(err):
		b, _ := json.Marshal(m)
		if err := writeAtomic(filepath.Join(st.Dir, "meta.json"), func(w io.Writer) error { _, err := w.Write(b); return err }); err != nil {
			return err
		}
		st.Meta = m
	default:
		return err
	}

	ents, err := os.ReadDir(st.Dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			os.Remove(filepath.Join(st.Dir, e.Name()))
		}
	}
	segs, err := segments(st.Dir)
	if err != nil {
		return err
	}
	for _, s := range segs {
		raw := filepath.Join(st.Dir, rawName(s.start))
		if !s.raw {
			os.Remove(raw)
			continue
		}
		f, err := os.OpenFile(raw, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		d := newDecoder(f)
		for {
			if _, err := d.next(); err != nil {
				break
			}
		}
		if fi, err := f.Stat(); err == nil && fi.Size() > d.done {
			err = f.Truncate(d.done)
			if err != nil {
				f.Close()
				return err
			}
		}
		f.Close()
	}
	return nil
}

// Append writes one raw reading.
func (st *Store) Append(rec *record) error {
	seg := st.Meta.segmentOf(rec.T)
	if st.cur == nil || seg != st.curStart {
		if err := st.openRaw(seg); err != nil {
			return err
		}
	}
	if _, err := st.cur.Write(st.enc.encode(rec)); err != nil {
		return err
	}
	if st.sync {
		return st.cur.Sync()
	}
	return nil
}

func (st *Store) openRaw(seg int64) error {
	if st.cur != nil {
		st.cur.Close()
		st.cur = nil
	}
	path := filepath.Join(st.Dir, rawName(seg))
	enc := newEncoder()
	// Continuing a segment: replay it so new frames reuse its keys and levels.
	if f, err := os.Open(path); err == nil {
		d := newDecoder(f)
		for {
			rec, err := d.next()
			if err != nil {
				break
			}
			enc.encode(rec)
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	st.cur, st.curStart, st.enc = f, seg, enc
	return nil
}

// Compact rolls up every raw segment that ends at or before cutoff.
func (st *Store) Compact(cutoff int64) error {
	if st.Meta.RollupStep == st.Meta.Step {
		return nil
	}
	segs, err := segments(st.Dir)
	if err != nil {
		return err
	}
	n := st.Meta.SegmentSeconds()
	for _, s := range segs {
		if s.stale {
			os.Remove(filepath.Join(st.Dir, rawName(s.start)))
		}
		if !s.raw || s.start+n > cutoff {
			continue
		}
		if s.start == st.curStart && st.cur != nil {
			st.cur.Close()
			st.cur = nil
		}
		if err := st.rollup(s.start); err != nil {
			return fmt.Errorf("roll up %s: %w", rawName(s.start), err)
		}
	}
	return nil
}

func (st *Store) rollup(start int64) error {
	raw := filepath.Join(st.Dir, rawName(start))
	f, err := os.Open(raw)
	if err != nil {
		return err
	}
	var buckets []*bucket
	byStart := map[int64]*bucket{}
	rs := int64(st.Meta.RollupStep)
	d := newDecoder(f)
	for {
		rec, err := d.next()
		if err != nil {
			break
		}
		bs := st.Meta.Origin + (rec.T-st.Meta.Origin)/rs*rs
		b := byStart[bs]
		if b == nil {
			b = newBucket(bs, int(rs))
			byStart[bs] = b
			buckets = append(buckets, b)
		}
		b.add(rec)
	}
	f.Close()
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].t < buckets[j].t })
	err = writeAtomic(filepath.Join(st.Dir, rollName(start)), func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 1<<16)
		z := gzip.NewWriter(bw)
		enc := newEncoder()
		for _, b := range buckets {
			if _, err := z.Write(enc.encode(b.record())); err != nil {
				return err
			}
		}
		if err := z.Close(); err != nil {
			return err
		}
		return bw.Flush()
	})
	if err != nil {
		return err
	}
	// Windows refuses to remove a file an export has open. The rollup already
	// takes precedence, and a later Compact removes the raw segment.
	os.Remove(raw)
	return nil
}

// Close releases the store and its lock.
func (st *Store) Close() error {
	if st.cur != nil {
		st.cur.Close()
	}
	return st.lock.Close()
}

// writeAtomic writes path through a synced temporary file and a rename.
func writeAtomic(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// bucket accumulates the readings of one rollup step.
type bucket struct {
	t, last    int64
	step       int
	readings   int
	noUsage    int
	skipped    int
	metricsErr string
	containers map[string]*cAgg
	workloads  map[string]wEntry
	nodes      map[string]nEntry
}

type cAgg struct {
	cEntry
	n              int64
	cpuSum, memSum int64
}

func newBucket(t int64, step int) *bucket {
	return &bucket{t: t, step: step, containers: map[string]*cAgg{}, workloads: map[string]wEntry{}, nodes: map[string]nEntry{}}
}

func (b *bucket) add(rec *record) {
	b.readings += rec.Readings
	b.noUsage += rec.NoUsage
	b.skipped = rec.Skipped
	if rec.MetricsErr != "" {
		b.metricsErr = rec.MetricsErr
	}
	for _, c := range rec.Containers {
		id := containerID(c.Key)
		a := b.containers[id]
		if a == nil {
			a = &cAgg{}
			b.containers[id] = a
		}
		usage := a.cEntry
		a.cEntry = c // levels: the latest reading's
		a.HasUsage, a.CPUUsage, a.MemoryUsage, a.CPUMax, a.MemMax = usage.HasUsage, usage.CPUUsage, usage.MemoryUsage, usage.CPUMax, usage.MemMax
		if c.HasUsage {
			a.HasUsage = true
			a.n++
			a.cpuSum += c.CPUUsage
			a.memSum += c.MemoryUsage
			a.CPUMax = max(a.CPUMax, c.CPUUsage)
			a.MemMax = max(a.MemMax, c.MemoryUsage)
		}
	}
	for _, w := range rec.Workloads {
		b.workloads[workloadID(w.Key)] = w
	}
	for _, n := range rec.Nodes {
		b.nodes[n.Name] = n
	}
}

func (b *bucket) record() *record {
	rec := &record{T: b.t, Step: b.step, Readings: b.readings, NoUsage: b.noUsage, Skipped: b.skipped, MetricsErr: b.metricsErr, HasMax: true}
	for _, a := range b.containers {
		c := a.cEntry
		if a.n > 0 {
			c.CPUUsage, c.MemoryUsage = divRound(a.cpuSum, a.n), divRound(a.memSum, a.n)
		}
		rec.Containers = append(rec.Containers, c)
	}
	for _, w := range b.workloads {
		rec.Workloads = append(rec.Workloads, w)
	}
	for _, n := range b.nodes {
		rec.Nodes = append(rec.Nodes, n)
	}
	sort.Slice(rec.Containers, func(i, j int) bool { return containerID(rec.Containers[i].Key) < containerID(rec.Containers[j].Key) })
	sort.Slice(rec.Workloads, func(i, j int) bool { return workloadID(rec.Workloads[i].Key) < workloadID(rec.Workloads[j].Key) })
	sort.Slice(rec.Nodes, func(i, j int) bool { return rec.Nodes[i].Name < rec.Nodes[j].Name })
	return rec
}
