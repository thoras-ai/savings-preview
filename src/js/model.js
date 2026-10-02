/* Reads the OTLP-JSON contract file and computes the savings figures the report shows.
 * Plain script: runs in the browser (exports on SP.model) and under node (module.exports). */
(function () {
  'use strict';

  var GIB = 1073741824;
  var HOURS_PER_YEAR = 8760;
  var PEAK_WINDOW_SECONDS = 900;
  var TARGET_HEADROOM = 1.1;
  var DEFAULT_NODE = { cores: 4, gib: 16, hourly: 0.192 };

  // On-demand Linux list prices in USD per hour, AWS us-east-1.
  var NODE_PRICES = {
    't3.medium': 0.0416, 't3.large': 0.0832, 't3.xlarge': 0.1664, 't3.2xlarge': 0.3328,
    'm5.large': 0.096, 'm5.xlarge': 0.192, 'm5.2xlarge': 0.384, 'm5.4xlarge': 0.768, 'm5.8xlarge': 1.536,
    'm6i.large': 0.096, 'm6i.xlarge': 0.192, 'm6i.2xlarge': 0.384, 'm6i.4xlarge': 0.768, 'm6i.8xlarge': 1.536,
    'c5.large': 0.085, 'c5.xlarge': 0.17, 'c5.2xlarge': 0.34, 'c5.4xlarge': 0.68, 'c5.9xlarge': 1.53,
    'c6i.large': 0.085, 'c6i.xlarge': 0.17, 'c6i.2xlarge': 0.34, 'c6i.4xlarge': 0.68,
    'r5.large': 0.126, 'r5.xlarge': 0.252, 'r5.2xlarge': 0.504, 'r5.4xlarge': 1.008, 'r5.8xlarge': 2.016,
    'r6i.large': 0.126, 'r6i.xlarge': 0.252, 'r6i.2xlarge': 0.504, 'r6i.4xlarge': 1.008
  };

  var M = {
    cpuUse: 'container.cpu.usage',
    memUse: 'container.memory.working_set',
    cpuPeak: 'thoras.container.cpu.usage.max',
    memPeak: 'thoras.container.memory.working_set.max',
    cpuReq: 'k8s.container.cpu.request.desired',
    memReq: 'k8s.container.memory.request.desired',
    cpuAlloc: 'k8s.node.cpu.allocatable',
    memAlloc: 'k8s.node.memory.allocatable'
  };
  var REQUIRED = [M.cpuUse, M.memUse, M.cpuReq, M.memReq];
  var REPLICA_METRICS = { deployment: 'k8s.deployment.pod.desired', statefulset: 'k8s.statefulset.pod.desired' };
  var FIELDS = ['reqCpu', 'reqMem', 'useCpu', 'useMem', 'tgtCpu', 'tgtMem', 'cur', 'opt'];

  function attrMap(resource) {
    var out = {};
    var list = (resource && resource.attributes) || [];
    for (var i = 0; i < list.length; i++) {
      var v = list[i].value || {};
      if ('stringValue' in v) out[list[i].key] = v.stringValue;
      else if ('intValue' in v) out[list[i].key] = Number(v.intValue);
      else if ('doubleValue' in v) out[list[i].key] = Number(v.doubleValue);
      else if (v.arrayValue) out[list[i].key] = (v.arrayValue.values || []).map(function (x) { return x.stringValue; });
    }
    return out;
  }

  function readSeries(metric) {
    var body = metric.gauge || metric.sum || {};
    var pts = body.dataPoints || [];
    var t = new Float64Array(pts.length);
    var v = new Float64Array(pts.length);
    for (var i = 0; i < pts.length; i++) {
      t[i] = Math.round(Number(pts[i].timeUnixNano) / 1e9);
      v[i] = pts[i].asDouble !== undefined ? Number(pts[i].asDouble) : Number(pts[i].asInt);
    }
    return { t: t, v: v };
  }

  function mean(arr, weights) {
    var s = 0, n = 0;
    for (var i = 0; i < arr.length; i++) {
      if (!isFinite(arr[i])) continue;
      var w = weights ? weights[i] : 1;
      s += arr[i] * w; n += w;
    }
    return n ? s / n : NaN;
  }

  // thoras.step_spans entries "START/END/STEP", or null when the resource has one step.
  function parseSpans(list) {
    if (!Array.isArray(list) || !list.length) return null;
    return list.map(function (x) {
      var p = String(x).split('/').map(Number);
      return { start: p[0], end: p[1], step: p[2] };
    });
  }

  // Spacing of a resource's points around time t.
  function stepAt(spans, t, step) {
    if (!spans) return step;
    for (var i = 0; i < spans.length; i++) if (t >= spans[i].start && t < spans[i].end) return spans[i].step;
    return step;
  }

  function parse(doc) {
    var rms = doc && doc.resourceMetrics;
    if (!Array.isArray(rms) || !rms.length) throw new Error('No resourceMetrics found. This is not a Savings Preview file.');
    var containers = [], workloads = {}, nodes = [], cluster = '';
    rms.forEach(function (rm) {
      var a = attrMap(rm.resource);
      if (a['k8s.cluster.name'] && !cluster) cluster = a['k8s.cluster.name'];
      var series = {};
      (rm.scopeMetrics || []).forEach(function (sm) {
        (sm.metrics || []).forEach(function (m) { series[m.name] = readSeries(m); });
      });
      var kind = a['k8s.deployment.name'] ? 'deployment' : a['k8s.statefulset.name'] ? 'statefulset' : null;
      var spans = parseSpans(a['thoras.step_spans']);
      var base = {
        attrs: a,
        step: a['thoras.step_seconds'] || 0,
        spans: spans,
        coarseStep: spans ? Math.max.apply(null, spans.map(function (x) { return x.step; })) : (a['thoras.step_seconds'] || 0),
        tier: a['thoras.fidelity_tier'] || 3,
        source: a['thoras.source'] || '',
        listedMissing: a['thoras.missing_metrics'] || [],
        series: series
      };
      if (a['k8s.node.name']) {
        nodes.push(Object.assign(base, {
          name: a['k8s.node.name'], hostType: a['host.type'] || '', region: a['cloud.region'] || '', capacity: a['thoras.node.capacity_type'] || 'unknown',
          cores: mean(series[M.cpuAlloc] ? series[M.cpuAlloc].v : []),
          gib: mean(series[M.memAlloc] ? series[M.memAlloc].v : []) / GIB
        }));
      } else if (kind) {
        var name = a['k8s.' + kind + '.name'], ns = a['k8s.namespace.name'] || '';
        var id = ns + '/' + kind + '/' + name;
        if (a['k8s.container.name']) {
          containers.push(Object.assign(base, { workloadId: id, ns: ns, kind: kind, workload: name, name: a['k8s.container.name'] }));
        } else {
          workloads[id] = Object.assign(base, { id: id, ns: ns, kind: kind, name: name });
        }
      }
    });
    if (!containers.length) throw new Error('The file has no container series, so there is nothing to analyze.');
    return { cluster: cluster, containers: containers, workloads: workloads, nodes: nodes };
  }

  function pricing(nodes) {
    var priced = nodes.filter(function (n) { return NODE_PRICES[n.hostType] !== undefined && n.cores > 0 && n.gib > 0; });
    if (!priced.length) {
      return {
        perCore: DEFAULT_NODE.hourly / DEFAULT_NODE.cores,
        perGiB: DEFAULT_NODE.hourly / DEFAULT_NODE.gib,
        basis: 'default node (4 cores, 16 GiB, $0.192 per hour)',
        priced: 0, nodes: nodes.length
      };
    }
    var cost = 0, cores = 0, gib = 0;
    var spot = priced.some(function (n) { return n.capacity === 'spot'; });
    var regions = priced.some(function (n) { return n.region && n.region !== 'us-east-1'; });
    var caveat = (spot ? '; spot nodes are priced at on-demand' : '') + (regions ? '; nodes outside us-east-1 are priced as if in it' : '');
    priced.forEach(function (n) { cost += NODE_PRICES[n.hostType]; cores += n.cores; gib += n.gib; });
    return {
      perCore: cost / cores, perGiB: cost / gib,
      basis: priced.length + ' of ' + nodes.length + ' nodes at AWS us-east-1 on-demand list prices' + caveat,
      priced: priced.length, nodes: nodes.length
    };
  }

  // Latest point at or before each grid time, only while it is younger than the
  // series step, or the step of the span the point is in.
  function resample(s, grid, step, spans) {
    var out = new Float64Array(grid.length).fill(NaN);
    if (!s || !s.t.length) return out;
    var j = 0;
    for (var i = 0; i < grid.length; i++) {
      var g = grid[i];
      while (j + 1 < s.t.length && s.t[j + 1] <= g) j++;
      var st = stepAt(spans, s.t[j], step);
      var span = st > 0 ? st : Infinity;
      if (s.t[j] <= g && g - s.t[j] < span) out[i] = s.v[j];
    }
    return out;
  }

  function unionTimes(lists) {
    var seen = new Set();
    lists.forEach(function (l) { for (var i = 0; i < l.length; i++) seen.add(l[i]); });
    return Float64Array.from(Array.from(seen).sort(function (a, b) { return a - b; }));
  }

  // Trailing max over the last `window` seconds, counting only valid points.
  function rollingMax(t, v, window) {
    var out = new Float64Array(v.length).fill(NaN);
    var dq = [], head = 0;
    for (var i = 0; i < v.length; i++) {
      if (!isFinite(v[i])) continue;
      while (dq.length > head && v[dq[dq.length - 1]] <= v[i]) dq.pop();
      dq.push(i);
      while (t[dq[head]] <= t[i] - window) head++;
      out[i] = v[dq[head]];
    }
    return out;
  }

  function missingRequired(c) {
    return REQUIRED.filter(function (n) { return !c.series[n] || !c.series[n].t.length; });
  }

  function analyzeWorkload(id, info, parts, rates) {
    var res = {
      id: id, ns: info.ns, kind: info.kind, name: info.name, containers: [],
      status: 'excluded', reason: '', series: null,
      annualSpend: 0, annualSavings: 0, savingsPct: 0, wasteCores: 0, wasteGiB: 0, reqCores: 0, reqGiB: 0
    };
    var usable = [];
    parts.forEach(function (c) {
      var miss = missingRequired(c);
      res.containers.push({ name: c.name, missing: miss, excluded: miss.length > 0 });
      if (!miss.length) usable.push(c);
    });
    if (!usable.length) {
      var all = [];
      res.containers.forEach(function (c) { c.missing.forEach(function (m) { if (all.indexOf(m) < 0) all.push(m); }); });
      res.reason = 'Missing ' + all.map(shortMetric).join(' and ') + ' data';
      return res;
    }
    var rep = info.workload && info.workload.series[REPLICA_METRICS[info.kind]];
    if (!rep || !rep.t.length) { res.reason = 'Missing replica count'; return res; }

    var lists = [];
    usable.forEach(function (c) { lists.push(c.series[M.cpuUse].t, c.series[M.memUse].t); });
    var grid = unionTimes(lists);
    var step = 0;
    usable.forEach(function (c) { step = Math.max(step, c.coarseStep); });
    var replicas = resample(rep, grid, info.workload.step, info.workload.spans);
    var n = grid.length;
    // Means are weighted by the step of the span each grid point is in, so a
    // rolled-up point counts for its whole step.
    var weight = new Float64Array(n);
    var s = {};
    FIELDS.forEach(function (f) { s[f] = new Float64Array(n); });
    var pkCpu = new Float64Array(n), pkMem = new Float64Array(n);
    usable.forEach(function (c, ci) {
      var at = function (name) { return resample(c.series[name], grid, c.step, c.spans); };
      var cu = at(M.cpuUse), mu = at(M.memUse), cr = at(M.cpuReq), mr = at(M.memReq);
      // Rolled-up points carry the peak of the readings they summarize.
      var pc = c.series[M.cpuPeak] ? at(M.cpuPeak) : cu, pm = c.series[M.memPeak] ? at(M.memPeak) : mu;
      for (var i = 0; i < n; i++) {
        if (ci === 0) { s.useCpu[i] = 0; s.useMem[i] = 0; s.reqCpu[i] = 0; s.reqMem[i] = 0; }
        weight[i] = Math.max(weight[i], stepAt(c.spans, grid[i], c.step) || 1);
        s.useCpu[i] += cu[i] * replicas[i];
        s.useMem[i] += mu[i] / GIB * replicas[i];
        s.reqCpu[i] += cr[i] * replicas[i];
        s.reqMem[i] += mr[i] / GIB * replicas[i];
        pkCpu[i] += (isFinite(pc[i]) ? pc[i] : cu[i]) * replicas[i];
        pkMem[i] += (isFinite(pm[i]) ? pm[i] : mu[i]) / GIB * replicas[i];
      }
    });
    var anyValid = false;
    for (var i = 0; i < n; i++) {
      var ok = isFinite(s.useCpu[i]) && isFinite(s.useMem[i]) && isFinite(s.reqCpu[i]) && isFinite(s.reqMem[i]);
      if (ok) anyValid = true;
      else { s.useCpu[i] = s.useMem[i] = s.reqCpu[i] = s.reqMem[i] = pkCpu[i] = pkMem[i] = NaN; }
    }
    if (!anyValid) { res.reason = 'No time with complete data'; return res; }
    var pc = rollingMax(grid, pkCpu, PEAK_WINDOW_SECONDS), pm = rollingMax(grid, pkMem, PEAK_WINDOW_SECONDS);
    for (var k = 0; k < n; k++) {
      s.tgtCpu[k] = pc[k] * TARGET_HEADROOM;
      s.tgtMem[k] = pm[k] * TARGET_HEADROOM;
      s.cur[k] = Math.max(s.reqCpu[k] * rates.perCore, s.reqMem[k] * rates.perGiB);
      s.opt[k] = Math.max(Math.min(s.reqCpu[k], s.tgtCpu[k]) * rates.perCore, Math.min(s.reqMem[k], s.tgtMem[k]) * rates.perGiB);
    }
    var wasteCpu = new Float64Array(n), wasteMem = new Float64Array(n);
    for (var m = 0; m < n; m++) {
      wasteCpu[m] = Math.max(0, s.reqCpu[m] - s.tgtCpu[m]);
      wasteMem[m] = Math.max(0, s.reqMem[m] - s.tgtMem[m]);
    }
    s.t = grid; s.step = step;
    res.series = s;
    res.status = res.containers.some(function (c) { return c.excluded; }) ? 'partial' : 'ok';
    if (res.status === 'partial') res.reason = 'Some containers lack data and are left out';
    res.reqCores = mean(s.reqCpu, weight);
    res.reqGiB = mean(s.reqMem, weight);
    res.annualSpend = mean(s.cur, weight) * HOURS_PER_YEAR;
    res.annualSavings = (mean(s.cur, weight) - mean(s.opt, weight)) * HOURS_PER_YEAR;
    res.savingsPct = res.annualSpend > 0 ? res.annualSavings / res.annualSpend : 0;
    res.wasteCores = mean(wasteCpu, weight);
    res.wasteGiB = mean(wasteMem, weight);
    return res;
  }

  function shortMetric(name) {
    if (name === M.cpuUse) return 'CPU usage';
    if (name === M.memUse) return 'memory usage';
    if (name === M.cpuReq) return 'CPU request';
    if (name === M.memReq) return 'memory request';
    return name;
  }

  // Sum workload series onto one grid. A bucket counts only when every workload has a value in it.
  function combine(list) {
    var step = 0, t0 = Infinity, t1 = -Infinity;
    list.forEach(function (w) {
      step = Math.max(step, w.series.step);
      t0 = Math.min(t0, w.series.t[0]);
      t1 = Math.max(t1, w.series.t[w.series.t.length - 1]);
    });
    if (!(step > 0)) step = 1;
    var nb = Math.floor((t1 - t0) / step) + 1;
    var out = { t: new Float64Array(nb), step: step };
    for (var b = 0; b < nb; b++) out.t[b] = t0 + b * step;
    var have = new Float64Array(nb);
    FIELDS.forEach(function (f) { out[f] = new Float64Array(nb); });
    list.forEach(function (w) {
      var sum = {}, cnt = new Float64Array(nb);
      FIELDS.forEach(function (f) { sum[f] = new Float64Array(nb); });
      var s = w.series;
      for (var i = 0; i < s.t.length; i++) {
        if (!isFinite(s.cur[i])) continue;
        var bi = Math.floor((s.t[i] - t0) / step);
        cnt[bi]++;
        FIELDS.forEach(function (f) { sum[f][bi] += s[f][i]; });
      }
      var first = Math.floor((s.t[0] - t0) / step), last = Math.floor((s.t[s.t.length - 1] - t0) / step);
      for (var j = 0; j < nb; j++) {
        if (j < first || j > last) { have[j]++; continue; }
        if (!cnt[j]) continue;
        have[j]++;
        FIELDS.forEach(function (f) { out[f][j] += sum[f][j] / cnt[j]; });
      }
    });
    for (var q = 0; q < nb; q++) {
      if (have[q] !== list.length) FIELDS.forEach(function (f) { out[f][q] = NaN; });
    }
    return out;
  }

  function fidelity(containers) {
    var tier = 1, step = 0, t0 = Infinity, t1 = -Infinity, sources = [];
    containers.forEach(function (c) {
      tier = Math.max(tier, c.tier);
      step = Math.max(step, c.coarseStep);
      if (c.source && sources.indexOf(c.source) < 0) sources.push(c.source);
      [M.cpuUse, M.memUse].forEach(function (n) {
        var s = c.series[n];
        if (s && s.t.length) { t0 = Math.min(t0, s.t[0]); t1 = Math.max(t1, s.t[s.t.length - 1] + stepAt(c.spans, s.t[s.t.length - 1], c.step)); }
      });
    });
    var span = isFinite(t0) ? t1 - t0 : 0;
    var points = step > 0 ? span / step : 0;
    return {
      tier: tier, stepSeconds: step, spanSeconds: span, snapshot: points <= 1,
      source: sources.join(', '), label: fidelityLabel(step, span, points <= 1)
    };
  }

  function formatStep(seconds) {
    if (seconds % 3600 === 0) return seconds / 3600 + '-hour';
    if (seconds % 60 === 0) return seconds / 60 + '-minute';
    return seconds + '-second';
  }

  function formatSpan(seconds) {
    var days = seconds / 86400;
    if (days >= 1) {
      var d = Math.round(days * 10) / 10;
      return d + (d === 1 ? ' day' : ' days');
    }
    var hours = Math.round(seconds / 360) / 10;
    return hours + (hours === 1 ? ' hour' : ' hours');
  }

  function fidelityLabel(step, span, snapshot) {
    if (snapshot) return 'Snapshot only';
    return formatSpan(span) + ' at ' + formatStep(step) + ' resolution';
  }

  function analyze(doc) {
    var data = parse(doc);
    var rates = pricing(data.nodes);
    var byWorkload = {}, order = [];
    data.containers.forEach(function (c) {
      if (!byWorkload[c.workloadId]) { byWorkload[c.workloadId] = []; order.push(c.workloadId); }
      byWorkload[c.workloadId].push(c);
    });
    var workloads = order.map(function (id) {
      var first = byWorkload[id][0];
      var info = { ns: first.ns, kind: first.kind, name: first.workload, workload: data.workloads[id] };
      return analyzeWorkload(id, info, byWorkload[id], rates);
    });
    var counted = workloads.filter(function (w) { return w.series; });
    var totals = { spend: 0, savings: 0, cores: 0, gib: 0, reqCores: 0, reqGiB: 0, workloads: workloads.length, counted: counted.length };
    counted.forEach(function (w) {
      totals.spend += w.annualSpend; totals.savings += w.annualSavings;
      totals.cores += w.wasteCores; totals.gib += w.wasteGiB;
      totals.reqCores += w.reqCores; totals.reqGiB += w.reqGiB;
    });
    return {
      cluster: data.cluster, fidelity: fidelity(data.containers), pricing: rates,
      workloads: workloads, totals: totals,
      series: counted.length ? combine(counted) : null
    };
  }

  var api = {
    analyze: analyze, parse: parse, pricing: pricing, resample: resample, rollingMax: rollingMax,
    fidelityLabel: fidelityLabel, constants: { GIB: GIB, HOURS_PER_YEAR: HOURS_PER_YEAR, PEAK_WINDOW_SECONDS: PEAK_WINDOW_SECONDS, TARGET_HEADROOM: TARGET_HEADROOM, DEFAULT_NODE: DEFAULT_NODE }
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).model = api;
})();
