package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/datadog"
	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/kube"
)

// Exit codes of savings-preview check.
const (
	checkReady   = 0 // run can read a source at tier 1
	checkLimited = 1 // run can read a source, at a lower tier or with gaps
	checkBlocked = 2 // run has no source it can read
)

var checkStatus = map[int]string{checkReady: "ready", checkLimited: "limited", checkBlocked: "cannot run"}

type checkReport struct {
	Status   string `json:"status"` // ready, limited or cannot run
	ExitCode int    `json:"exit_code"`
	Summary  string `json:"summary"`

	Kubernetes    *kubeCheck       `json:"kubernetes,omitempty"`
	Access        []accessCheck    `json:"access,omitempty"`
	MetricsServer *metricsCheck    `json:"metrics_server,omitempty"`
	Stores        []storeCheck     `json:"stores,omitempty"`
	StoreSearch   string           `json:"store_search,omitempty"` // why no store was found
	Datadog       *datadogCheck    `json:"datadog,omitempty"`
	Source        *sourceCheck     `json:"source"`
	Order         []sourceStepInfo `json:"order"`
}

type kubeCheck struct {
	Needed        bool     `json:"needed"`
	Error         string   `json:"error,omitempty"` // the kubeconfig could not be read
	Context       string   `json:"context,omitempty"`
	Cluster       string   `json:"cluster,omitempty"`
	Server        string   `json:"server,omitempty"`
	KubeUser      string   `json:"kubeconfig_user,omitempty"`
	Identity      string   `json:"identity,omitempty"` // the user the API server sees
	Groups        []string `json:"groups,omitempty"`
	IdentityError string   `json:"identity_error,omitempty"`

	short     string // Error in a few words, for the source lines
	explained bool   // Error says what is wrong with the kubeconfig and what to do
}

type accessCheck struct {
	Mode     string `json:"mode"` // prometheus or poll
	Check    string `json:"check"`
	Verb     string `json:"verb"`
	Group    string `json:"group"`
	Resource string `json:"resource"`
	// Namespace is "*" for all namespaces and "" for a cluster-scoped resource.
	Namespace string `json:"namespace"`
	Name      string `json:"name,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

type metricsCheck struct {
	Answers bool   `json:"answers"`
	Pods    int    `json:"pods"`
	Error   string `json:"error,omitempty"`
}

type storeCheck struct {
	Store         string    `json:"store"`
	FoundBy       string    `json:"found_by"`
	Answers       bool      `json:"answers"`
	Usable        bool      `json:"usable"`
	Error         string    `json:"error,omitempty"`
	Version       string    `json:"version,omitempty"`
	Earliest      time.Time `json:"earliest,omitzero"`
	HistorySecs   int64     `json:"history_seconds,omitempty"`
	FullHistory   bool      `json:"full_history,omitempty"`
	UsageFromPods bool      `json:"usage_from_pods,omitempty"`
	Tier          int       `json:"tier,omitempty"`
	Warnings      []string  `json:"warnings,omitempty"`
}

type datadogCheck struct {
	KeysGiven bool   `json:"keys_given"`
	Used      bool   `json:"used"` // run would try Datadog
	Site      string `json:"site,omitempty"`
	Valid     bool   `json:"valid"`
	Error     string `json:"error,omitempty"`
}

type sourceCheck struct {
	Name     string   `json:"name,omitempty"` // datadog, prometheus or poll; empty when nothing can be read
	From     string   `json:"from,omitempty"`
	Tier     int      `json:"tier,omitempty"`
	TierText string   `json:"tier_text,omitempty"`
	Why      string   `json:"why"`
	Limits   []string `json:"limits,omitempty"`
}

// sourceStepInfo is one source in the order run tries them.
type sourceStepInfo struct {
	Source string `json:"source"`
	Usable bool   `json:"usable"`
	Note   string `json:"note"`
	// Final is set when run stops at this source whether or not it works.
	Final bool `json:"final,omitempty"`
}

// checkEnv makes the reads check reports on. Tests replace it.
type checkEnv struct {
	conn     *kube.Conn
	connErr  error
	whoami   func(context.Context) (*kube.Identity, error)
	canI     func(context.Context, kube.Access) (bool, string, error)
	metrics  func(context.Context) (int, error)
	discover func(context.Context) (*prometheus.Discovery, error)
	// answer checks that a store answers queries; probe makes the checks a
	// pull makes before it reads the history.
	answer func(context.Context, prometheus.Candidate) error
	probe  func(context.Context, prometheus.Candidate) (*prometheus.Report, error)
	// direct opens the store at --url.
	direct  func(context.Context) (name string, answer func(context.Context) error, probe func(context.Context) (*prometheus.Report, error), err error)
	datadog func(context.Context) (site string, err error)
}

func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	o, finish := addRunFlags(fs)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	color := fs.String("color", "auto", "color the output: auto (when printing to a terminal and NO_COLOR is unset), always or never")
	timeout := fs.Duration("timeout", time.Minute, "give up on the reads still running after this long")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview check [flags]

Answers, before anything runs, whether "savings-preview run" with the same
flags can read usage data, from where, and at what fidelity tier: the identity
the kubeconfig gives, what that identity may do (as kubectl auth can-i), whether
metrics-server answers, which Prometheus-compatible stores answer and how much
history they hold, and whether the Datadog keys are valid. It only reads, and
pulls no history.

Exit status: 0 ready, 1 usable but limited, 2 cannot run. Flags that only
change what run writes, such as --out and --no-open, are accepted and ignored.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return checkBlocked
	}
	if err := finish(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return checkBlocked
	}
	switch *color {
	case "auto", "always", "never":
	default:
		fmt.Fprintf(os.Stderr, "error: --color must be auto, always or never, not %q\n", *color)
		return checkBlocked
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "error: --timeout must be positive")
		return checkBlocked
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	r := check(ctx, o, newCheckEnv(o), time.Now())
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return checkBlocked
		}
	} else {
		printCheck(os.Stdout, r, useColor(*color, os.Stdout))
	}
	return r.ExitCode
}

func newCheckEnv(o *runOptions) checkEnv {
	var e checkEnv
	e.conn, e.connErr = kube.Connect(o.kubeconfig, o.context)
	e.connErr = kube.WithCommand(e.connErr, "savings-preview check")
	if e.conn != nil {
		cs := e.conn.Clientset
		e.whoami = func(ctx context.Context) (*kube.Identity, error) { return kube.WhoAmI(ctx, cs) }
		e.canI = func(ctx context.Context, a kube.Access) (bool, string, error) { return kube.CanI(ctx, cs, a) }
		e.metrics = func(ctx context.Context) (int, error) { return kube.PodMetrics(ctx, cs.Discovery().RESTClient()) }
		e.discover = func(ctx context.Context) (*prometheus.Discovery, error) {
			return prometheus.Discover(ctx, cs, e.conn.Dynamic)
		}
		client := func(c prometheus.Candidate) (*prometheus.Client, error) {
			px, err := e.conn.Proxy(c.Namespace, c.Service, c.Port, c.Scheme, c.Prefix)
			if err != nil {
				return nil, err
			}
			return &prometheus.Client{G: px, Selector: o.promSelector, MaxRange: o.direct.maxRange, Retries: 1}, nil
		}
		e.answer = func(ctx context.Context, c prometheus.Candidate) error {
			cl, err := client(c)
			if err != nil {
				return err
			}
			return cl.Check(ctx)
		}
		e.probe = func(ctx context.Context, c prometheus.Candidate) (*prometheus.Report, error) {
			cl, err := client(c)
			if err != nil {
				return nil, err
			}
			return prometheus.Probe(ctx, prometheus.Options{Client: cl, Days: o.days, Step: o.step})
		}
	}
	e.direct = func(ctx context.Context) (string, func(context.Context) error, func(context.Context) (*prometheus.Report, error), error) {
		g, store, err := o.direct.open(ctx)
		if err != nil {
			return "", nil, nil, err
		}
		cl := store.NewClient(g, o.promSelector)
		cl.Retries = 1
		probe := func(ctx context.Context) (*prometheus.Report, error) {
			rep, err := prometheus.Probe(ctx, prometheus.Options{Client: cl, Days: o.days, Step: o.step,
				AdaptiveMetrics: store.Name == prometheus.BackendGrafanaCloud, GKE: o.direct.gkeMode(store),
				CloudWatch: cloudWatchRegion(store)})
			return rep, directHint(err, store)
		}
		return storeName(store), func(ctx context.Context) error { return directHint(cl.Check(ctx), store) }, probe, nil
	}
	e.datadog = func(ctx context.Context) (string, error) {
		c, err := o.datadog.Client()
		if err != nil {
			return "", err
		}
		c.Retries = 1
		c.Log = func(string, ...any) {}
		return c.Base, datadog.Hint(c.Validate(ctx))
	}
	return e
}

// promAccess is the access the prometheus source needs. The proxy rows name
// each store found; without one they ask about every service.
func promAccess(target *prometheus.Candidate, stores []prometheus.Candidate) []kube.Access {
	var out []kube.Access
	if target == nil {
		out = append(out,
			kube.Access{Verb: "list", Resource: "services", AllNamespaces: true},
			kube.Access{Verb: "list", Group: "monitoring.coreos.com", Resource: "prometheuses", AllNamespaces: true},
			kube.Access{Verb: "list", Group: "monitoring.coreos.com", Resource: "prometheusagents", AllNamespaces: true},
		)
	} else {
		stores = []prometheus.Candidate{*target}
	}
	for _, c := range stores {
		out = append(out, kube.Access{Verb: "get", Resource: "services", Subresource: "proxy", Namespace: c.Namespace, Name: proxyName(c)})
	}
	if len(stores) == 0 {
		out = append(out, kube.Access{Verb: "get", Resource: "services", Subresource: "proxy", AllNamespaces: true})
	}
	return out
}

// proxyName is the name the API server authorizes a service proxy request
// by, as in deploy/rbac/prometheus-namespaced.yaml.
func proxyName(c prometheus.Candidate) string {
	n := c.Service + ":" + c.Port
	if c.Scheme == "https" {
		n = "https:" + n
	}
	return n
}

var pollAccess = []kube.Access{
	{Verb: "list", Resource: "pods", AllNamespaces: true},
	{Verb: "list", Group: "apps", Resource: "replicasets", AllNamespaces: true},
	{Verb: "list", Group: "apps", Resource: "deployments", AllNamespaces: true},
	{Verb: "list", Group: "apps", Resource: "statefulsets", AllNamespaces: true},
	{Verb: "list", Resource: "nodes"},
	{Verb: "list", Group: "metrics.k8s.io", Resource: "pods", AllNamespaces: true},
}

// check makes the reads and decides which source run would pick.
func check(ctx context.Context, o *runOptions, e checkEnv, now time.Time) *checkReport {
	r := &checkReport{}
	useStore := o.direct.url == "" && (o.source == "auto" || o.source == "prometheus")
	usePoll := o.source == "poll" || (o.source == "auto" && o.direct.url == "")

	// Kubernetes
	r.Kubernetes = &kubeCheck{Needed: useStore || usePoll}
	if e.connErr != nil {
		r.Kubernetes.Error, r.Kubernetes.short = e.connErr.Error(), kubeShort(e.connErr)
		var ce *kube.ConfigError
		r.Kubernetes.explained = errors.As(e.connErr, &ce) && ce.Kind != ""
	} else if e.conn != nil {
		k := r.Kubernetes
		k.Context, k.Cluster, k.KubeUser = e.conn.Context, e.conn.ClusterName, e.conn.User
		if e.conn.Config != nil {
			k.Server = e.conn.Config.Host
		}
	}
	cluster := e.conn != nil && e.connErr == nil && r.Kubernetes.Needed
	if cluster {
		if id, err := e.whoami(ctx); err != nil {
			r.Kubernetes.IdentityError = err.Error()
		} else {
			r.Kubernetes.Identity, r.Kubernetes.Groups = id.User, id.Groups
		}
	}

	// Datadog
	if o.useDatadog || o.datadog.KeysGiven() {
		d := &datadogCheck{KeysGiven: o.datadog.KeysGiven(), Used: o.useDatadog}
		if d.Used {
			site, err := e.datadog(ctx)
			d.Site = site
			if err != nil {
				d.Error = err.Error()
			} else {
				d.Valid = true
			}
		}
		r.Datadog = d
	}

	// Stores
	var found []prometheus.Candidate
	if o.direct.url != "" {
		r.Stores = append(r.Stores, checkDirect(ctx, o, e, now))
	} else if useStore && cluster {
		if o.target != nil {
			found = []prometheus.Candidate{*o.target}
		} else if d, err := e.discover(ctx); err != nil {
			r.StoreSearch = "could not look for a store: " + err.Error()
		} else {
			found = d.Candidates
			if len(found) == 0 {
				r.StoreSearch = "no Prometheus-compatible store found in the cluster"
				if len(d.Agents) > 0 {
					a := d.Agents[0]
					r.StoreSearch = fmt.Sprintf("the only Prometheus found (%s/%s) runs in agent mode and keeps no history to query; its data goes to %s",
						a.Namespace, a.Name, strings.Join(a.RemoteWrite, ", "))
				}
			}
		}
		for _, c := range found {
			foundBy := c.Reason
			r.Stores = append(r.Stores, checkStore(ctx, o, now, foundBy, c.String(),
				func(ctx context.Context) error { return e.answer(ctx, c) },
				func(ctx context.Context) (*prometheus.Report, error) { return e.probe(ctx, c) }))
		}
	}

	// Access and metrics-server
	if cluster {
		if useStore {
			for _, a := range promAccess(o.target, found) {
				r.Access = append(r.Access, canI(ctx, e, "prometheus", a, a.Group == "monitoring.coreos.com"))
			}
		}
		if usePoll {
			for _, a := range pollAccess {
				r.Access = append(r.Access, canI(ctx, e, "poll", a, false))
			}
			m := &metricsCheck{}
			if n, err := e.metrics(ctx); err != nil {
				m.Error = err.Error()
			} else {
				m.Answers, m.Pods = true, n
				if n == 0 {
					m.Error = "metrics-server answers but returns no pod usage yet"
				}
			}
			r.MetricsServer = m
		}
	}

	decide(r, o, usePoll)
	return r
}

func canI(ctx context.Context, e checkEnv, mode string, a kube.Access, optional bool) accessCheck {
	ns := a.Namespace
	if a.AllNamespaces {
		ns = "*"
	}
	c := accessCheck{Mode: mode, Check: a.String(), Verb: a.Verb, Group: a.Group, Resource: a.Resource, Namespace: ns, Name: a.Name, Optional: optional}
	if a.Subresource != "" {
		c.Resource += "/" + a.Subresource
	}
	ok, reason, err := e.canI(ctx, a)
	c.Allowed, c.Reason = ok, reason
	if err != nil {
		c.Error = err.Error()
	}
	return c
}

func checkDirect(ctx context.Context, o *runOptions, e checkEnv, now time.Time) storeCheck {
	if o.cluster == "" && e.connErr != nil {
		return storeCheck{Store: "store at " + prometheus.RedactURL(o.direct.url), FoundBy: "--url",
			Error: "--cluster-name is required with --url when the kubeconfig cannot be used: " + kubeShort(e.connErr) + " (see Kubernetes)"}
	}
	name, answer, probe, err := e.direct(ctx)
	if err != nil {
		return storeCheck{Store: "store at " + prometheus.RedactURL(o.direct.url), FoundBy: "--url", Error: err.Error()}
	}
	return checkStore(ctx, o, now, "--url", name, answer, probe)
}

func checkStore(ctx context.Context, o *runOptions, now time.Time, foundBy, name string,
	answer func(context.Context) error, probe func(context.Context) (*prometheus.Report, error)) storeCheck {
	s := storeCheck{Store: name, FoundBy: foundBy}
	if err := answer(ctx); err != nil {
		s.Error = "does not answer queries: " + err.Error()
		return s
	}
	s.Answers = true
	rep, err := probe(ctx)
	if rep != nil {
		s.Version = rep.Version
	}
	if err != nil {
		s.Error = "cannot be used: " + err.Error()
		return s
	}
	if rep.NoRequests {
		s.Error = fmt.Sprintf("cannot be used: it has no container with all of %s, so no savings can be computed from it (%s)",
			strings.Join(contract.RequiredContainerMetrics, ", "), strings.Join(rep.Warnings, "; "))
		return s
	}
	s.Usable = true
	s.Earliest, s.FullHistory, s.UsageFromPods, s.Warnings = rep.Start.UTC(), rep.FullHistory, rep.UsageFromPods, rep.Warnings
	span := now.Sub(rep.Start)
	if rep.FullHistory {
		span = time.Duration(o.days) * 24 * time.Hour
	}
	s.HistorySecs = int64(span / time.Second)
	s.Tier = contract.TierFor(int(o.step/time.Second), int64(span/time.Second))
	return s
}

// decide fills Source, Order, Status and Summary, following the order in
// which collect tries the sources.
func decide(r *checkReport, o *runOptions, usePoll bool) {
	var limits []string
	pick := func(name, from, why string, tier int) {
		r.Source = &sourceCheck{Name: name, From: from, Tier: tier, TierText: tierText(tier), Why: why, Limits: limits}
	}
	add := func(src string, usable bool, note string, final bool) {
		r.Order = append(r.Order, sourceStepInfo{Source: src, Usable: usable, Note: note, Final: final})
	}

	if o.direct.url != "" {
		s := r.Stores[0]
		add("the "+s.Store, s.Usable, storeNote(s), true)
		if s.Usable {
			limits = append(limits, storeLimits(s, o)...)
			pick("prometheus", s.Store, "--url names the store", s.Tier)
		}
		finish(r)
		return
	}

	if r.Datadog == nil && o.source == "auto" && o.target == nil {
		add("Datadog", false, "no keys given", false)
	}
	if r.Datadog != nil && r.Datadog.Used {
		d := r.Datadog
		final := o.source == "datadog"
		note := "keys are valid"
		if !d.Valid {
			note = d.Error
		}
		add("Datadog", d.Valid, note, final)
		if d.Valid {
			tier := contract.TierFor(int(o.step/time.Second), int64(o.days)*86400)
			if tier > 1 {
				limits = append(limits, fmt.Sprintf("--days %d at --step %s gives at most tier %d", o.days, shortDur(o.step), tier))
			}
			why := "Datadog keys are given and valid"
			if final {
				why = "--source datadog"
			}
			pick("datadog", "Datadog at "+d.Site, why, tier)
			r.Source.Why += "; only the run itself finds out whether Datadog holds --days of history at --step, which the tier assumes, and whether its series hold one cluster or --selector picks one (several clusters stop the run)"
			finish(r)
			return
		}
		if final {
			finish(r)
			return
		}
	}

	if o.source == "auto" || o.source == "prometheus" {
		final := o.source == "prometheus" || o.target != nil
		switch {
		case r.Kubernetes.Error != "":
			add("a store in the cluster", false, r.Kubernetes.short+" (see Kubernetes)", final)
		case r.StoreSearch != "":
			add("a store in the cluster", false, r.StoreSearch, final)
		}
		for _, s := range r.Stores {
			add(s.Store, s.Usable, storeNote(s), final)
			if s.Usable {
				limits = append(limits, storeLimits(s, o)...)
				why := "the first store found that answers and holds the series the savings math needs"
				if o.target != nil {
					why = "--target names the store"
				}
				pick("prometheus", s.Store, why, s.Tier)
				finish(r)
				return
			}
		}
		if final {
			finish(r)
			return
		}
	}

	if usePoll {
		usable, note := pollReady(r)
		add("polling metrics-server", usable, note, true)
		if usable {
			tier := 3
			if o.duration > 0 {
				tier = contract.TierFor(int(pollStep(o)/time.Second), int64(o.duration/time.Second))
			}
			if tier > 1 {
				limits = append(limits, pollLimit(o, tier))
			}
			why := "no other source can be read"
			if o.source == "poll" {
				why = "--source poll"
			}
			pick("poll", "metrics-server, by polling", why, tier)
		}
	}
	finish(r)
}

func pollReady(r *checkReport) (bool, string) {
	if r.Kubernetes.Error != "" {
		return false, r.Kubernetes.short + " (see Kubernetes)"
	}
	var denied []string
	for _, a := range r.Access {
		if a.Mode == "poll" && !a.Allowed && a.Error == "" {
			denied = append(denied, a.Check)
		}
	}
	if len(denied) > 0 {
		return false, "the identity may not " + strings.Join(denied, ", ")
	}
	if m := r.MetricsServer; m == nil || !m.Answers || m.Pods == 0 {
		if m == nil {
			return false, "metrics-server was not checked"
		}
		return false, "metrics-server: " + m.Error
	}
	return true, "access allowed and metrics-server answers"
}

func pollLimit(o *runOptions, tier int) string {
	if o.duration == 0 {
		return "polling gives tier 3 until it has 7 days of readings"
	}
	return fmt.Sprintf("polling for %s gives tier %d; tier 2 needs 7 days of readings, tier 1 needs 14 days at an --interval of 1m or less", shortDur(o.duration), tier)
}

func (s storeCheck) versionText() string {
	if s.Version == "" {
		return ""
	}
	return ", version " + s.Version
}

func (s storeCheck) history() time.Duration { return time.Duration(s.HistorySecs) * time.Second }

func storeNote(s storeCheck) string {
	if s.Usable {
		return fmt.Sprintf("answers, %s of history, tier %d", spanText(s.history()), s.Tier)
	}
	return s.Error
}

func storeLimits(s storeCheck, o *runOptions) []string {
	// Probe warns when the store holds less than --days.
	out := append([]string{}, s.Warnings...)
	if s.Tier > 1 && s.FullHistory {
		out = append(out, fmt.Sprintf("--days %d at --step %s gives at most tier %d", o.days, shortDur(o.step), s.Tier))
	}
	if s.UsageFromPods {
		out = append(out, "the store has no per-container usage series, so container usage is the pod's usage split by requests")
	}
	return out
}

func finish(r *checkReport) {
	switch {
	case r.Source == nil:
		r.ExitCode = checkBlocked
		r.Source = &sourceCheck{Why: "no source can be read"}
		reason := "no source can be read"
		if n := len(r.Order); n > 0 {
			last := r.Order[n-1]
			reason = last.Source + ": " + last.Note
		}
		r.Summary = reason
	case r.Source.Tier > 1 || len(r.Source.Limits) > 0:
		r.ExitCode = checkLimited
		r.Summary = fmt.Sprintf("run reads %s; expect tier %d (%s)", r.Source.From, r.Source.Tier, r.Source.TierText)
		if len(r.Source.Limits) > 0 {
			r.Summary += ": " + r.Source.Limits[0]
		}
	default:
		r.ExitCode = checkReady
		r.Summary = fmt.Sprintf("run reads %s; expect tier 1 (%s)", r.Source.From, r.Source.TierText)
	}
	r.Status = checkStatus[r.ExitCode]
}

func useColor(mode string, f *os.File) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

type painter bool

func (p painter) paint(code, s string) string {
	if !p {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p painter) good(s string) string { return p.paint("32", s) }
func (p painter) warn(s string) string { return p.paint("33", s) }
func (p painter) bad(s string) string  { return p.paint("31", s) }
func (p painter) bold(s string) string { return p.paint("1", s) }

func printCheck(w io.Writer, r *checkReport, color bool) {
	p := painter(color)
	k := r.Kubernetes
	fmt.Fprintln(w, p.bold("Kubernetes"))
	switch {
	case k.Error != "" && k.Needed:
		first, rest, _ := strings.Cut(k.Error, "\n")
		if k.explained {
			fmt.Fprintf(w, "  %s\n", p.bad(first))
		} else {
			fmt.Fprintf(w, "  %s %s\n", p.bad("cannot read the kubeconfig:"), first)
		}
		if rest != "" {
			fmt.Fprintln(w, indent(rest, "  "))
		}
	case k.Error != "" && k.explained:
		fmt.Fprintf(w, "  not needed; %s\n", strings.ReplaceAll(k.Error, "\n", "\n  "))
	case k.Error != "":
		fmt.Fprintf(w, "  not needed; cannot read the kubeconfig (%s)\n", k.Error)
	default:
		fmt.Fprintf(w, "  context %s, cluster %s, API server %s\n", k.Context, k.Cluster, k.Server)
		switch {
		case !k.Needed:
			fmt.Fprintln(w, "  not needed with these flags: nothing is read from the cluster")
		case k.IdentityError != "":
			fmt.Fprintf(w, "  identity: kubeconfig user %s (the API server did not say: %s)\n", k.KubeUser, k.IdentityError)
		default:
			fmt.Fprintf(w, "  identity: %s", k.Identity)
			if len(k.Groups) > 0 {
				fmt.Fprintf(w, " (groups %s)", strings.Join(k.Groups, ", "))
			}
			fmt.Fprintln(w)
		}
	}

	if len(r.Access) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.bold("Access (kubectl auth can-i)"))
		width := 0
		for _, a := range r.Access {
			width = max(width, len(a.Check))
		}
		// A connection problem fails every review the same way; say it once.
		common := r.Access[0].Error
		for _, a := range r.Access {
			if a.Error != common {
				common = ""
			}
		}
		for _, a := range r.Access {
			var verdict string
			switch {
			case common != "":
				verdict = p.warn("unknown")
			case a.Error != "":
				verdict = p.warn("unknown: " + a.Error)
			case a.Allowed:
				verdict = p.good("yes")
			case a.Optional:
				verdict = p.warn("no") + " (optional)"
			default:
				verdict = p.bad("no")
			}
			fmt.Fprintf(w, "  %-11s %-*s  %s\n", a.Mode, width, a.Check, verdict)
		}
		if common != "" {
			fmt.Fprintf(w, "  every check failed: %s\n", common)
		}
	}

	if m := r.MetricsServer; m != nil {
		fmt.Fprintln(w)
		fmt.Fprint(w, p.bold("metrics-server")+": ")
		switch {
		case m.Answers && m.Pods > 0:
			fmt.Fprintln(w, p.good("answers"))
		case m.Answers:
			fmt.Fprintln(w, p.warn(m.Error))
		default:
			fmt.Fprintln(w, p.bad("cannot read: ")+m.Error)
		}
	}

	if len(r.Stores) > 0 || r.StoreSearch != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.bold("Prometheus-compatible stores"))
		if r.StoreSearch != "" {
			fmt.Fprintf(w, "  %s\n", r.StoreSearch)
		}
		for _, s := range r.Stores {
			fmt.Fprintf(w, "  %s\n", s.Store)
			switch {
			case !s.Answers:
				fmt.Fprintf(w, "    %s\n", p.bad(s.Error))
			case !s.Usable:
				fmt.Fprintf(w, "    %s%s; %s\n", p.good("answers"), s.versionText(), p.warn(s.Error))
			default:
				v := s.versionText()
				hist := fmt.Sprintf("data from %s (%s)", s.Earliest.Format(time.RFC3339), spanText(s.history()))
				if s.FullHistory {
					hist = "data for all " + spanText(s.history()) + " asked for"
				}
				fmt.Fprintf(w, "    %s%s; %s; tier %d\n", p.good("answers"), v, hist, s.Tier)
				if s.UsageFromPods {
					fmt.Fprintln(w, "    note: no per-container usage series; container usage would be the pod's usage split by requests")
				}
				for _, m := range s.Warnings {
					fmt.Fprintf(w, "    %s %s\n", p.warn("warning:"), m)
				}
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprint(w, p.bold("Datadog")+": ")
	switch d := r.Datadog; {
	case d == nil:
		fmt.Fprintf(w, "no keys (%s and %s, or --api-key-file and --app-key-file)\n", datadog.EnvAPIKey, datadog.EnvAppKey)
	case !d.Used:
		fmt.Fprintln(w, "keys given but not used: --url, --target or --source names another source")
	case d.Valid:
		fmt.Fprintf(w, "%s at %s\n", p.good("keys are valid"), d.Site)
	default:
		fmt.Fprintln(w, p.bad(d.Error))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, p.bold("Source"))
	for i, s := range r.Order {
		mark := p.bad("no ")
		if s.Usable {
			mark = p.good("yes")
		}
		fmt.Fprintf(w, "  %d. %s %s: %s\n", i+1, mark, s.Source, s.Note)
	}
	if s := r.Source; s.Name != "" {
		fmt.Fprintf(w, "  run would read: %s\n  why: %s\n  expected tier: %d (%s)\n", s.From, s.Why, s.Tier, s.TierText)
		for _, l := range s.Limits {
			fmt.Fprintf(w, "  limit: %s\n", l)
		}
	}

	fmt.Fprintln(w)
	status := map[int]func(string) string{checkReady: p.good, checkLimited: p.warn, checkBlocked: p.bad}[r.ExitCode]
	fmt.Fprintf(w, "%s: %s\n", status(r.Status), r.Summary)
}

// kubeShort is the kubeconfig error in a few words.
func kubeShort(err error) string {
	var ce *kube.ConfigError
	if errors.As(err, &ce) {
		return ce.Short()
	}
	return "cannot read the kubeconfig"
}

// indent prefixes each line of s.
func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
