package prometheus

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseSelector(t *testing.T) {
	cases := []struct{ in, want string }{
		{``, ``},
		{`cluster="prod-us"`, `cluster="prod-us"`},
		{`{cluster="prod-us"}`, `cluster="prod-us"`},
		{` cluster = "prod-us" , region=~"us-.*" `, `cluster="prod-us",region=~"us-.*"`},
		{`cluster=prod-us`, `cluster="prod-us"`},
		{`cluster=prod-us,env!=dev`, `cluster="prod-us",env!="dev"`},
		{`cluster='prod-us'`, `cluster="prod-us"`},
		{"cluster=`a\\b`", `cluster="a\\b"`},
		{`cluster=""`, `cluster=""`},
		{`name="a,b",x!~"c|d"`, `name="a,b",x!~"c|d"`},
		{`name="say \"hi\""`, `name="say \"hi\""`},
	}
	for _, c := range cases {
		got, err := ParseSelector(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{`cluster`, `1x="a"`, `__name__="up"`, `cluster="a`, `cluster=~"("`, `a="b" c="d"`, `cluster<"a"`, `cluster=a"b`} {
		if got, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: want an error, got %q", bad, got)
		}
	}
}

func TestWithSelector(t *testing.T) {
	m := `cluster="a"`
	cases := []struct{ in, want string }{
		{`up{}`, `up{cluster="a"}`},
		{`up{job="x"}`, `up{cluster="a",job="x"}`},
		{`rate(x{a="{"}[5m]) + y{} offset 60s`, `rate(x{cluster="a",a="{"}[5m]) + y{cluster="a"} offset 60s`},
		{`label_replace(z{}, "w", "$1", "o", "(.+)")`, `label_replace(z{cluster="a"}, "w", "$1", "o", "(.+)")`},
		{`vector(1)`, `vector(1)`},
	}
	for _, c := range cases {
		if got := withSelector(c.in, m); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	if got := withSelector(`up{}`, ""); got != `up{}` {
		t.Errorf("empty selector changed the query: %s", got)
	}
}

// A metric name written without braces would escape the selector.
func TestEveryMetricInEveryQueryHasBraces(t *testing.T) {
	var qs []string
	for _, kind := range []string{Deployment, StatefulSet} {
		qs = append(qs, CPUUsage(kind, time.Minute), MemoryUsage(kind), PodCPUUsage(kind, time.Minute), PodMemoryUsage(kind),
			OOMKills(kind, time.Minute), Requests(kind, "cpu"), Limits(kind, "memory"), Replicas(kind))
	}
	qs = append(qs, NodeAllocatable("cpu"), UsageProbe, CPUProbe, RequestsProbe, OwnerProbe, ReplicaSetProbe, PhaseProbe,
		PodUsageProbe, ContainerProbe, RestartsProbe, UnlabeledPods)
	qs = append(qs, ClusterValues(time.Hour)...)
	info, labels := NodeInfoAt(time.Hour)
	qs = append(qs, info, labels)
	metric := regexp.MustCompile(`\b(container|kube|prometheus)_[a-z_]+\b`)
	for _, q := range qs {
		for _, loc := range metric.FindAllStringIndex(q, -1) {
			if !strings.HasPrefix(q[loc[1]:], "{") {
				t.Errorf("metric %s has no braces:\n%s", q[loc[0]:loc[1]], q)
			}
		}
	}
}
