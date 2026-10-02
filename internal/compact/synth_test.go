package compact

import (
	"fmt"
	"io"
	"math"
	"math/rand/v2"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// synth describes a synthetic cluster: workloads deployments with an app and a
// proxy container each, on 20 nodes, at step seconds for days.
type synth struct {
	workloads int
	days      int
	step      int
	start     int64
}

const synthStart = 1789516800 // 2026-09-16T00:00:00Z

// write streams the cluster as a contract file, one resource at a time.
// Usage follows a daily cycle with noise and short spikes. Some workloads
// change their requests partway, some scale with load, and some have an
// outage with no data.
func (s synth) write(w io.Writer) error {
	cw := contract.NewWriter(w)
	n := s.days * 86400 / s.step
	tier := contract.TierFor(s.step, int64(s.days)*86400)
	common := contract.Common{Cluster: "synthetic", Source: "prometheus", StepSeconds: s.step, Tier: tier}
	start := s.start
	if start == 0 {
		start = synthStart
	}
	at := func(i int) int64 { return start + int64(i*s.step) }
	for wi := range s.workloads {
		rng := rand.New(rand.NewPCG(uint64(wi), 7))
		ns, name := fmt.Sprintf("ns-%d", wi%25), fmt.Sprintf("app-%d", wi)
		scales := wi%5 == 0
		gapFrom, gapTo := -1, -1
		if wi%7 == 3 {
			gapFrom = n/3 + rng.IntN(n/10)
			gapTo = gapFrom + 3*3600/s.step
		}
		inGap := func(i int) bool { return i >= gapFrom && i < gapTo }
		load := func(i int) float64 {
			h := float64(at(i)%86400) / 3600
			return 0.5 + 0.4*math.Sin((h-6)/24*2*math.Pi)
		}
		replicas := func(i int) float64 {
			if scales {
				return math.Max(1, math.Round(4*load(i)))
			}
			return 2
		}
		var reps []contract.Point
		for i := range n {
			if !inGap(i) {
				reps = append(reps, contract.Point{T: at(i), V: replicas(i)})
			}
		}
		if err := cw.Resource(contract.Resource{Common: common, Namespace: ns, Kind: "deployment", Workload: name,
			Metrics: []contract.Series{{Name: contract.DeploymentPods, Points: reps}}}); err != nil {
			return err
		}
		for ci, cname := range []string{"app", "proxy"} {
			base := []float64{0.2 + rng.Float64(), 0.02 + 0.05*rng.Float64()}[ci]
			memBase := []float64{512 << 20, 64 << 20}[ci] * (1 + rng.Float64())
			cpuReq := []float64{1, 0.1}[ci]
			memReq := []float64{2 << 30, 128 << 20}[ci]
			change := -1
			if wi%3 == 1 {
				change = n/2 + rng.IntN(n/4)
			}
			var cu, mu, cr, mr, cl, ml []contract.Point
			for i := range n {
				if inGap(i) {
					continue
				}
				t := at(i)
				l := load(i)
				if scales {
					l = l / replicas(i) * 2
				}
				c := base * l * (0.85 + 0.3*rng.Float64())
				if rng.IntN(500) == 0 {
					c *= 2 + 3*rng.Float64()
				}
				m := memBase * (0.9 + 0.2*l + 0.05*rng.Float64())
				req := cpuReq
				if change >= 0 && i >= change {
					req = cpuReq / 2
				}
				cu = append(cu, contract.Point{T: t, V: math.Round(c*1e6) / 1e6})
				mu = append(mu, contract.Point{T: t, V: math.Round(m)})
				cr = append(cr, contract.Point{T: t, V: req})
				mr = append(mr, contract.Point{T: t, V: memReq})
				if ci == 0 {
					cl = append(cl, contract.Point{T: t, V: 2 * cpuReq})
					ml = append(ml, contract.Point{T: t, V: 2 * memReq})
				}
			}
			ms := []contract.Series{
				{Name: contract.CPUUsage, Points: cu}, {Name: contract.MemoryWorkingSet, Points: mu},
				{Name: contract.CPURequest, Points: cr}, {Name: contract.MemoryRequest, Points: mr},
			}
			if ci == 0 {
				ms = append(ms, contract.Series{Name: contract.CPULimit, Points: cl}, contract.Series{Name: contract.MemoryLimit, Points: ml})
			}
			if err := cw.Resource(contract.Resource{Common: common, Namespace: ns, Kind: "deployment", Workload: name,
				Container: cname, Metrics: ms}); err != nil {
				return err
			}
		}
	}
	for ni := range 20 {
		var cpu, mem []contract.Point
		for i := range n {
			cpu = append(cpu, contract.Point{T: at(i), V: 15.89})
			mem = append(mem, contract.Point{T: at(i), V: 62 << 30})
		}
		if err := cw.Resource(contract.Resource{Common: common,
			Node:    &contract.Node{Name: fmt.Sprintf("node-%d", ni), InstanceType: "m5.4xlarge", Provider: "aws", Region: "us-east-1", CapacityType: "on_demand"},
			Metrics: []contract.Series{{Name: contract.NodeCPU, Points: cpu}, {Name: contract.NodeMemory, Points: mem}}}); err != nil {
			return err
		}
	}
	return cw.Close()
}
