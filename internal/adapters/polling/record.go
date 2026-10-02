package polling

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"sort"
	"strings"
)

// A record is one reading, or the rollup of several readings over one step.
type record struct {
	T          int64
	Step       int
	Readings   int // readings summarized; 1 for a raw reading
	NoUsage    int // readings in which no container had usage
	Skipped    int
	MetricsErr string
	HasMax     bool // container entries carry peaks, which only rollups do
	Containers []cEntry
	Workloads  []wEntry
	Nodes      []nEntry
}

type cEntry struct {
	Key ContainerKey
	Levels
	CPUMax, MemMax int64
}

type wEntry struct {
	Key      WorkloadKey
	Replicas int64
}

type nEntry struct {
	NodeInfo
}

func fromReading(t int64, step int, r Reading) *record {
	rec := &record{T: t, Step: step, Readings: 1, Skipped: r.Skipped, MetricsErr: r.MetricsErr}
	if r.NoUsage {
		rec.NoUsage = 1
	}
	for k, lv := range r.Containers {
		rec.Containers = append(rec.Containers, cEntry{Key: k, Levels: lv})
	}
	for k, n := range r.Replicas {
		rec.Workloads = append(rec.Workloads, wEntry{k, int64(n)})
	}
	for _, n := range r.Nodes {
		rec.Nodes = append(rec.Nodes, nEntry{n})
	}
	sort.Slice(rec.Containers, func(i, j int) bool { return containerID(rec.Containers[i].Key) < containerID(rec.Containers[j].Key) })
	sort.Slice(rec.Workloads, func(i, j int) bool { return workloadID(rec.Workloads[i].Key) < workloadID(rec.Workloads[j].Key) })
	sort.Slice(rec.Nodes, func(i, j int) bool { return rec.Nodes[i].Name < rec.Nodes[j].Name })
	return rec
}

func workloadID(k WorkloadKey) string { return k.Namespace + "\x00" + k.Kind + "\x00" + k.Name }
func containerID(k ContainerKey) string {
	return workloadID(k.WorkloadKey) + "\x00" + k.Container
}

// Segment files are a sequence of frames: a uvarint payload length, the
// payload, and the payload's CRC-32. A frame that is cut short or fails its
// checksum ends the segment, so a torn write at the tail is dropped.
//
// Payloads define keys ('K'), set levels that persist until changed ('L'),
// and hold readings ('R'). Keys and levels are scoped to one segment.

const (
	kindContainer = 'c'
	kindWorkload  = 'w'
	kindNode      = 'n'
)

type buf []byte

func (b buf) u(v uint64) buf { return binary.AppendUvarint(b, v) }
func (b buf) i(v int64) buf {
	if v < 0 {
		v = 0
	}
	return b.u(uint64(v))
}
func (b buf) s(v string) buf { return append(b.u(uint64(len(v))), v...) }

func frame(payload []byte) []byte {
	out := binary.AppendUvarint(make([]byte, 0, len(payload)+8), uint64(len(payload)))
	out = append(out, payload...)
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(payload))
}

// encoder turns records into frames for one segment.
type encoder struct {
	ids    map[string]uint64
	levels map[uint64]string
}

func newEncoder() *encoder {
	return &encoder{ids: map[string]uint64{}, levels: map[uint64]string{}}
}

func (e *encoder) id(key string, out *[]byte) uint64 {
	if id, ok := e.ids[key]; ok {
		return id
	}
	id := uint64(len(e.ids))
	e.ids[key] = id
	*out = append(*out, frame(buf{'K'}.u(id).s(key))...)
	return id
}

func (e *encoder) encode(rec *record) []byte {
	var out []byte
	var lv buf = buf{'L'}
	nLevels := 0
	setLevel := func(id uint64, v buf) {
		if e.levels[id] == string(v) {
			return
		}
		e.levels[id] = string(v)
		lv = lv.u(id).s(string(v))
		nLevels++
	}
	r := buf{'R'}.i(rec.T).u(uint64(rec.Step)).u(uint64(rec.Readings)).u(uint64(rec.NoUsage)).u(uint64(rec.Skipped)).s(rec.MetricsErr)
	flags := byte(0)
	if rec.HasMax {
		flags = 1
	}
	r = append(r, flags)

	r = r.u(uint64(len(rec.Containers)))
	for _, c := range rec.Containers {
		id := e.id(string(kindContainer)+containerID(c.Key), &out)
		var lim byte
		if c.HasCPULimit {
			lim |= 1
		}
		if c.HasMemLimit {
			lim |= 2
		}
		v := buf{}.i(c.CPURequest).i(c.MemoryRequest)
		v = append(v, lim)
		v = v.i(c.CPULimit).i(c.MemoryLimit)
		setLevel(id, v)
		r = r.u(id)
		if !c.HasUsage {
			r = append(r, 0)
			continue
		}
		r = append(r, 1)
		r = r.i(c.CPUUsage).i(c.MemoryUsage)
		if rec.HasMax {
			r = r.i(c.CPUMax).i(c.MemMax)
		}
	}
	r = r.u(uint64(len(rec.Workloads)))
	for _, w := range rec.Workloads {
		id := e.id(string(kindWorkload)+workloadID(w.Key), &out)
		setLevel(id, buf{}.i(w.Replicas))
		r = r.u(id)
	}
	r = r.u(uint64(len(rec.Nodes)))
	for _, n := range rec.Nodes {
		a := n.NodeAttrs
		id := e.id(string(kindNode)+strings.Join([]string{a.Name, a.InstanceType, a.Provider, a.Region, a.CapacityType}, "\x00"), &out)
		setLevel(id, buf{}.i(n.CPU).i(n.Memory))
		r = r.u(id)
	}
	if nLevels > 0 {
		out = append(out, frame(lv)...)
	}
	return append(out, frame(r)...)
}

// decoder reads frames of one segment.
type decoder struct {
	r    *bufio.Reader
	read int64 // bytes of complete, valid frames consumed
	// done is the length up to the end of the last complete reading. Key and
	// level frames written for a reading whose own frame was torn lie past it.
	done   int64
	keys   map[uint64]string
	levels map[uint64][]byte
	cut    map[string][]string
	// want, when set, limits records to the entries whose key it accepts.
	want   func(key string) bool
	wanted map[uint64]bool
}

func newDecoder(r io.Reader) *decoder {
	return &decoder{r: bufio.NewReaderSize(r, 1<<16), keys: map[uint64]string{}, levels: map[uint64][]byte{}, cut: map[string][]string{}, wanted: map[uint64]bool{}}
}

func (d *decoder) keep(id uint64) bool {
	if d.want == nil {
		return true
	}
	w, ok := d.wanted[id]
	if !ok {
		w = d.want(d.keys[id])
		d.wanted[id] = w
	}
	return w
}

var errTorn = errors.New("torn frame")

func (d *decoder) frame() ([]byte, error) {
	n, err := binary.ReadUvarint(d.r)
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil || n > 1<<28 {
		return nil, errTorn
	}
	p := make([]byte, n+4)
	if _, err := io.ReadFull(d.r, p); err != nil {
		return nil, errTorn
	}
	if crc32.ChecksumIEEE(p[:n]) != binary.LittleEndian.Uint32(p[n:]) {
		return nil, errTorn
	}
	d.read += int64(uvarintLen(n)) + int64(n) + 4
	return p[:n], nil
}

func uvarintLen(n uint64) int { return len(binary.AppendUvarint(nil, n)) }

type rd struct {
	b   []byte
	bad bool
}

func (r *rd) u() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.b = r.b[n:]
	return v
}
func (r *rd) i() int64 { return int64(r.u()) }
func (r *rd) byte() byte {
	if len(r.b) == 0 {
		r.bad = true
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}
func (r *rd) s() string {
	n := r.u()
	if uint64(len(r.b)) < n {
		r.bad = true
		return ""
	}
	v := string(r.b[:n])
	r.b = r.b[n:]
	return v
}

func (d *decoder) parts(key string) []string {
	if p, ok := d.cut[key]; ok {
		return p
	}
	p := strings.Split(key[1:], "\x00")
	d.cut[key] = p
	return p
}

// next returns the next record, io.EOF at a clean end, or errTorn when the
// rest of the segment is unreadable.
func (d *decoder) next() (*record, error) {
	for {
		p, err := d.frame()
		if err != nil {
			return nil, err
		}
		r := &rd{b: p[1:]}
		switch p[0] {
		case 'K':
			id := r.u()
			d.keys[id] = r.s()
		case 'L':
			for len(r.b) > 0 && !r.bad {
				id := r.u()
				d.levels[id] = []byte(r.s())
			}
		case 'R':
			rec := &record{T: r.i(), Step: int(r.u()), Readings: int(r.u()), NoUsage: int(r.u()), Skipped: int(r.u()), MetricsErr: r.s()}
			rec.HasMax = r.byte() == 1
			n := int(r.u())
			if n > len(r.b) {
				return nil, errTorn
			}
			if d.want == nil {
				rec.Containers = make([]cEntry, 0, n)
			}
			for i := 0; i < n && !r.bad; i++ {
				id := r.u()
				key, lvb := d.keys[id], d.levels[id]
				if key == "" || key[0] != kindContainer || lvb == nil {
					return nil, errTorn
				}
				if !d.keep(id) {
					if r.byte() == 1 {
						r.u()
						r.u()
						if rec.HasMax {
							r.u()
							r.u()
						}
					}
					continue
				}
				kp := d.parts(key)
				c := cEntry{Key: ContainerKey{WorkloadKey{kp[0], kp[1], kp[2]}, kp[3]}}
				lr := &rd{b: lvb}
				c.CPURequest, c.MemoryRequest = lr.i(), lr.i()
				lim := lr.byte()
				c.CPULimit, c.MemoryLimit = lr.i(), lr.i()
				c.HasCPULimit, c.HasMemLimit = lim&1 != 0, lim&2 != 0
				if r.byte() == 1 {
					c.HasUsage = true
					c.CPUUsage, c.MemoryUsage = r.i(), r.i()
					if rec.HasMax {
						c.CPUMax, c.MemMax = r.i(), r.i()
					}
				}
				rec.Containers = append(rec.Containers, c)
			}
			n = int(r.u())
			for i := 0; i < n && !r.bad; i++ {
				id := r.u()
				key, lvb := d.keys[id], d.levels[id]
				if key == "" || key[0] != kindWorkload || lvb == nil {
					return nil, errTorn
				}
				if !d.keep(id) {
					continue
				}
				kp := d.parts(key)
				rec.Workloads = append(rec.Workloads, wEntry{WorkloadKey{kp[0], kp[1], kp[2]}, (&rd{b: lvb}).i()})
			}
			n = int(r.u())
			for i := 0; i < n && !r.bad; i++ {
				id := r.u()
				key, lvb := d.keys[id], d.levels[id]
				if key == "" || key[0] != kindNode || lvb == nil {
					return nil, errTorn
				}
				if !d.keep(id) {
					continue
				}
				kp := d.parts(key)
				lr := &rd{b: lvb}
				rec.Nodes = append(rec.Nodes, nEntry{NodeInfo{NodeAttrs{kp[0], kp[1], kp[2], kp[3], kp[4]}, lr.i(), lr.i()}})
			}
			if r.bad {
				return nil, errTorn
			}
			d.done = d.read
			return rec, nil
		default:
			return nil, errTorn
		}
		if r.bad {
			return nil, errTorn
		}
	}
}
