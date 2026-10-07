/* Reads the OTLP-JSON contract file and computes the savings figures the report shows.
 * Plain script: runs in the browser (exports on SP.model) and under node (module.exports). */
(function () {
  'use strict';

  var GIB = 1073741824;
  var HOURS_PER_YEAR = 8760;
  var PEAK_WINDOW_SECONDS = 900;
  var TARGET_HEADROOM = 1.1;
  var DEFAULT_NODE = { cores: 4, gib: 16, hourly: 0.192 };
  var CONTRACT_VERSION = '1.0.0';
  // A file that holds several clusters, or a cluster that could not be read.
  var FLEET_VERSION = '1.4.0';

  // The Thoras product's node price table, imported from its SQL migrations by
  // scripts/import_node_prices.py. The page builders inline it before this script.
  var PRICES = typeof module !== 'undefined' && module.exports ? require('../../internal/savings/node_prices.json') : self.SP.nodePriceTable;
  var CLOUD_ORDER = ['aws', 'gcp', 'azure'];
  var CLOUD_NAMES = { aws: 'AWS', gcp: 'Google Cloud', azure: 'Azure' };
  var CUSTOM_SIZE = /-custom-(\d+)-(\d+)(-ext)?$/;

  var M = {
    cpuUse: 'container.cpu.usage',
    memUse: 'container.memory.working_set',
    cpuPeak: 'thoras.container.cpu.usage.max',
    memPeak: 'thoras.container.memory.working_set.max',
    cpuReq: 'k8s.container.cpu.request.desired',
    memReq: 'k8s.container.memory.request.desired',
    cpuAlloc: 'k8s.node.cpu.allocatable',
    memAlloc: 'k8s.node.memory.allocatable',
    cpuLim: 'k8s.container.cpu.limit.desired',
    memLim: 'k8s.container.memory.limit.desired',
    oom: 'thoras.container.oom_kills'
  };
  var REQUIRED = [M.cpuUse, M.memUse, M.cpuReq, M.memReq];
  var REPLICA_METRICS = { deployment: 'k8s.deployment.pod.desired', statefulset: 'k8s.statefulset.pod.desired' };
  var FIELDS = ['reqCpu', 'reqMem', 'useCpu', 'useMem', 'peakCpu', 'peakMem', 'tgtCpu', 'tgtMem', 'cur', 'opt', 'useCost'];

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

  // Over indexes [from, to) when given, else the whole array.
  function mean(arr, weights, from, to) {
    var s = 0, n = 0, end = to === undefined ? arr.length : to;
    for (var i = from || 0; i < end; i++) {
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

  // Resources without a cluster name belong to the first cluster named, as
  // they did when a file held one.
  function parse(doc) {
    var rms = doc && doc.resourceMetrics;
    if (!Array.isArray(rms) || !rms.length) throw new Error('No resourceMetrics found. This is not a Savings Preview file.');
    var containers = [], workloads = {}, nodes = [], cluster = '', contract = { version: '', schemaUrl: '' };
    var clusters = [], failed = [];
    rms.forEach(function (rm) {
      var a = attrMap(rm.resource);
      if (a['k8s.cluster.name'] && !cluster) cluster = a['k8s.cluster.name'];
      if (rm.schemaUrl && !contract.schemaUrl) contract.schemaUrl = String(rm.schemaUrl);
      var series = {};
      (rm.scopeMetrics || []).forEach(function (sm) {
        var version = sm.scope && sm.scope.version ? String(sm.scope.version) : '';
        if (version && version !== CONTRACT_VERSION && version !== FLEET_VERSION) throw new Error('This file is contract version ' + version + ' and this page reads only ' + CONTRACT_VERSION + ' and ' + FLEET_VERSION + '. Collect the data again with the current savings-preview.');
        if (version && (!contract.version || version === FLEET_VERSION)) contract.version = version;
        (sm.metrics || []).forEach(function (m) { series[m.name] = readSeries(m); });
      });
      var kind = a['k8s.deployment.name'] ? 'deployment' : a['k8s.statefulset.name'] ? 'statefulset' : null;
      if (a['thoras.cluster.error'] && !kind && !a['k8s.node.name'] && !a['k8s.container.name']) {
        failed.push({ name: String(a['k8s.cluster.name'] || ''), reason: String(a['thoras.cluster.error']) });
        return;
      }
      var name0 = a['k8s.cluster.name'] || '';
      if (clusters.indexOf(name0) < 0) clusters.push(name0);
      var spans = parseSpans(a['thoras.step_spans']);
      var held = {};
      (Array.isArray(a['thoras.held_metrics']) ? a['thoras.held_metrics'] : []).forEach(function (n) { held[n] = true; });
      var coarseStep = spans ? Math.max.apply(null, spans.map(function (x) { return x.step; })) : (a['thoras.step_seconds'] || 0);
      var base = {
        attrs: a,
        cluster: name0,
        step: a['thoras.step_seconds'] || 0,
        spans: spans,
        coarseStep: coarseStep,
        sourceStep: a['thoras.source_step_seconds'] > 0 && a['thoras.source_step_seconds'] < coarseStep ? a['thoras.source_step_seconds'] : coarseStep,
        held: held,
        tier: a['thoras.fidelity_tier'] || 3,
        source: a['thoras.source'] || '',
        notes: Array.isArray(a['thoras.fidelity_notes']) ? a['thoras.fidelity_notes'] : [],
        listedMissing: a['thoras.missing_metrics'] || [],
        series: series
      };
      if (a['k8s.node.name']) {
        nodes.push(Object.assign(base, {
          name: a['k8s.node.name'], hostType: a['host.type'] || '', provider: a['cloud.provider'] || '', region: a['cloud.region'] || '', capacity: a['thoras.node.capacity_type'] || 'unknown',
          cores: levelMean(base, M.cpuAlloc),
          gib: levelMean(base, M.memAlloc) / GIB
        }));
      } else if (kind) {
        var name = a['k8s.' + kind + '.name'], ns = a['k8s.namespace.name'] || '';
        var id = ns + '/' + kind + '/' + name;
        if (a['k8s.container.name']) {
          containers.push(Object.assign(base, { workloadId: id, ns: ns, kind: kind, workload: name, name: a['k8s.container.name'] }));
        } else {
          workloads[name0 + '\u0000' + id] = Object.assign(base, { id: id, ns: ns, kind: kind, name: name });
        }
      }
    });
    // Unnamed resources join the first named cluster.
    var unnamed = clusters.indexOf('');
    if (unnamed >= 0 && cluster) {
      clusters.splice(unnamed, 1);
      containers.concat(nodes).forEach(function (r) { if (!r.cluster) r.cluster = cluster; });
      Object.keys(workloads).forEach(function (k) {
        var w = workloads[k];
        if (!w.cluster) { delete workloads[k]; w.cluster = cluster; workloads[cluster + '\u0000' + w.id] = w; }
      });
    }
    if (!containers.length) {
      if (failed.length) throw new Error('No cluster in this file could be read: ' + failed.map(function (f) { return f.name + ' (' + f.reason + ')'; }).join('; ') + '.');
      throw new Error('The file has no container series, so there is nothing to analyze.');
    }
    return { cluster: cluster, clusters: clusters, failed: failed, contract: contract, containers: containers, workloads: workloads, nodes: nodes };
  }

  function customFor(table, type) {
    for (var i = 0; i < table.custom.length; i++) if (type.indexOf(table.custom[i][0]) === 0) return table.custom[i];
    return null;
  }

  // How one node is priced, the way the product's average node query does:
  // the cloud's table by instance type, else a custom family's per-unit
  // prices, and the spot price for a spot node. The product detects no Azure
  // spot nodes, so they are priced at on-demand. reason is empty when the node
  // is priced, or says why it is left out: type, cloud, size or spot.
  // internal/savings priceNode is the Go port.
  function priceNode(n) {
    var p = { cloud: n.provider || '', cores: 0, gib: 0, hourly: 0, spot: false, custom: false, inferred: false, azureSpot: false, otherRegion: false, reason: '' };
    var type = n.hostType;
    if (!type || type === 'unknown') { p.reason = 'type'; return p; }
    if (CLOUD_ORDER.indexOf(p.cloud) < 0) {
      if (p.cloud && p.cloud !== 'unknown') { p.reason = 'cloud'; return p; }
      p.cloud = ''; p.inferred = true;
      for (var i = 0; i < CLOUD_ORDER.length; i++) {
        var c = PRICES[CLOUD_ORDER[i]];
        if (Object.prototype.hasOwnProperty.call(c.types, type) || customFor(c, type)) { p.cloud = CLOUD_ORDER[i]; break; }
      }
      if (!p.cloud) { p.reason = 'type'; return p; }
    }
    var t = PRICES[p.cloud];
    p.spot = n.capacity === 'spot';
    if (p.spot && p.cloud === 'azure') { p.spot = false; p.azureSpot = true; }
    p.otherRegion = !!n.region && n.region !== 'unknown' && n.region !== t.region;
    if (Object.prototype.hasOwnProperty.call(t.types, type)) {
      var row = t.types[type], price = p.spot ? row[3] : row[2];
      p.cores = row[0]; p.gib = row[1] / 1024;
      if (price === null) { p.reason = 'spot'; return p; }
      p.hourly = price;
      return p;
    }
    var cu = customFor(t, type);
    if (!cu) { p.reason = 'type'; return p; }
    p.custom = true;
    var m = CUSTOM_SIZE.exec(type);
    p.cores = m ? Number(m[1]) : n.cores;
    p.gib = m ? Number(m[2]) / 1024 : n.gib;
    if (!(p.cores > 0) || !(p.gib > 0)) { p.reason = 'size'; return p; }
    var vcpu = p.spot ? cu[3] : cu[1], gib = p.spot ? cu[4] : cu[2];
    if (vcpu === null || gib === null) { p.reason = 'spot'; return p; }
    p.hourly = p.cores * vcpu + p.gib * gib;
    return p;
  }

  function listJoin(items) {
    return items.length < 2 ? items.join('') : items.slice(0, -1).join(', ') + ' and ' + items[items.length - 1];
  }

  // The rates the product would set from these nodes: the average priced
  // node's hourly cost over its vCPUs, and over its GiB. With no node priced
  // it falls back to the product's default node.
  function pricing(nodes) {
    var priced = 0, cost = 0, cores = 0, gib = 0, clouds = {}, spot = false, azureSpot = false, regions = false;
    nodes.forEach(function (n) {
      var p = priceNode(n);
      if (p.reason) return;
      priced++;
      cost += p.hourly; cores += p.cores; gib += p.gib;
      clouds[p.cloud] = true;
      spot = spot || p.spot; azureSpot = azureSpot || p.azureSpot; regions = regions || p.otherRegion;
    });
    if (!priced) {
      return {
        perCore: DEFAULT_NODE.hourly / DEFAULT_NODE.cores,
        perGiB: DEFAULT_NODE.hourly / DEFAULT_NODE.gib,
        basis: 'default node (4 cores, 16 GiB, $0.192 per hour)',
        priced: 0, nodes: nodes.length
      };
    }
    var used = CLOUD_ORDER.filter(function (c) { return clouds[c]; });
    var where = used.map(function (c) { return CLOUD_NAMES[c] + ' ' + PRICES[c].region; });
    var basis = priced + ' of ' + nodes.length + ' nodes at ' + listJoin(where) + ' ' + (spot ? 'on-demand and spot' : 'on-demand') + ' list prices';
    if (azureSpot) basis += '; Azure spot nodes are priced at on-demand';
    if (regions) basis += used.length === 1 ? '; nodes outside ' + PRICES[used[0]].region + ' are priced as if in it' : '; nodes outside those regions are priced as if in them';
    return { perCore: cost / cores, perGiB: cost / gib, basis: basis, priced: priced, nodes: nodes.length };
  }

  // Mean of a level series over the time it covers. A held series weights each
  // point by the time until the next one.
  function levelMean(r, name) {
    var s = r.series[name];
    if (!s) return mean([]);
    if (!r.held[name]) return mean(s.v);
    var w = new Float64Array(s.t.length);
    for (var i = 0; i < s.t.length; i++) w[i] = i + 1 < s.t.length ? s.t[i + 1] - s.t[i] : stepAt(r.spans, s.t[i], r.step);
    return mean(s.v, w);
  }

  // Latest point at or before each grid time, only while it is younger than the
  // series step, or the step of the span the point is in. A held series has
  // points only where its value changes, so every point but the last stays
  // current until the next one.
  function resample(s, grid, step, spans, held) {
    var out = new Float64Array(grid.length).fill(NaN);
    if (!s || !s.t.length) return out;
    var j = 0;
    for (var i = 0; i < grid.length; i++) {
      var g = grid[i];
      while (j + 1 < s.t.length && s.t[j + 1] <= g) j++;
      var st = stepAt(spans, s.t[j], step);
      var span = st > 0 ? st : Infinity;
      if (s.t[j] <= g && (g - s.t[j] < span || (held && j + 1 < s.t.length))) out[i] = s.v[j];
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

  // Aligns a workload's containers on one time grid and sums them, scaled by
  // replicas. Everything here is independent of the levers, so it runs once
  // per file.
  function prepareWorkload(id, info, parts) {
    var p = {
      id: id, ns: info.ns, kind: info.kind, name: info.name, containers: [], notes: [], status: 'excluded', reason: '',
      s: null, weight: null, wasteCpu: null, wasteMem: null, peaks: null,
      replicas: null, limCpu: null, limMem: null, oom: oomKills(parts)
    };
    var usable = [];
    parts.forEach(function (c) {
      var miss = missingRequired(c);
      p.containers.push({ name: c.name, missing: miss, excluded: miss.length > 0 });
      if (!miss.length) usable.push(c);
    });
    if (!usable.length) {
      var all = [];
      p.containers.forEach(function (c) { c.missing.forEach(function (m) { if (all.indexOf(m) < 0) all.push(m); }); });
      p.reason = 'Missing ' + all.map(shortMetric).join(' and ') + ' data';
      return p;
    }
    var rep = info.workload && info.workload.series[REPLICA_METRICS[info.kind]];
    if (!rep || !rep.t.length) { p.reason = 'Missing replica count'; return p; }

    var lists = [];
    usable.forEach(function (c) { lists.push(c.series[M.cpuUse].t, c.series[M.memUse].t); });
    var grid = unionTimes(lists);
    var step = 0;
    usable.forEach(function (c) { step = Math.max(step, c.coarseStep); });
    var repName = REPLICA_METRICS[info.kind];
    var replicas = resample(rep, grid, info.workload.step, info.workload.spans, !!info.workload.held[repName]);
    var n = grid.length;
    // Means are weighted by the step of the span each grid point is in, so a
    // rolled-up point counts for its whole step.
    var weight = new Float64Array(n);
    var s = {};
    FIELDS.forEach(function (f) { s[f] = new Float64Array(n); });
    var pkCpu = s.peakCpu, pkMem = s.peakMem;
    // Per-pod limits, summed over containers; null when any container has none.
    var limCpu = new Float64Array(n), limMem = new Float64Array(n);
    var hasLimCpu = true, hasLimMem = true;
    // Containers with both usage values at each point. One without them while
    // others have them is not running then: an init container, a check that
    // runs now and then, or a sidecar that has exited. It adds no usage and
    // no request there, since the file carries requests only where a
    // container has usage.
    var running = new Int32Array(n);
    usable.forEach(function (c, ci) {
      var at = function (name) { return resample(c.series[name], grid, c.step, c.spans, !!c.held[name]); };
      var cu = at(M.cpuUse), mu = at(M.memUse), cr = at(M.cpuReq), mr = at(M.memReq);
      // Rolled-up points carry the peak of the readings they summarize.
      var pc = c.series[M.cpuPeak] ? at(M.cpuPeak) : cu, pm = c.series[M.memPeak] ? at(M.memPeak) : mu;
      var up = new Uint8Array(n);
      for (var i = 0; i < n; i++) {
        if (ci === 0) { s.useCpu[i] = 0; s.useMem[i] = 0; s.reqCpu[i] = 0; s.reqMem[i] = 0; }
        weight[i] = Math.max(weight[i], stepAt(c.spans, grid[i], c.step) || 1);
        if (!isFinite(cu[i]) || !isFinite(mu[i])) continue;
        up[i] = 1;
        running[i]++;
        s.useCpu[i] += cu[i] * replicas[i];
        s.useMem[i] += mu[i] / GIB * replicas[i];
        s.reqCpu[i] += cr[i] * replicas[i];
        s.reqMem[i] += mr[i] / GIB * replicas[i];
        pkCpu[i] += (isFinite(pc[i]) ? pc[i] : cu[i]) * replicas[i];
        pkMem[i] += (isFinite(pm[i]) ? pm[i] : mu[i]) / GIB * replicas[i];
      }
      if (c.series[M.cpuLim] && c.series[M.cpuLim].t.length) {
        var lc = at(M.cpuLim);
        for (var a = 0; a < n; a++) if (up[a]) limCpu[a] += lc[a];
      } else hasLimCpu = false;
      if (c.series[M.memLim] && c.series[M.memLim].t.length) {
        var lm = at(M.memLim);
        for (var b = 0; b < n; b++) if (up[b]) limMem[b] += lm[b] / GIB;
      } else hasLimMem = false;
    });
    var anyValid = false;
    for (var i = 0; i < n; i++) {
      var ok = running[i] > 0 && isFinite(s.useCpu[i]) && isFinite(s.useMem[i]) && isFinite(s.reqCpu[i]) && isFinite(s.reqMem[i]);
      if (ok) anyValid = true;
      else { s.useCpu[i] = s.useMem[i] = s.reqCpu[i] = s.reqMem[i] = pkCpu[i] = pkMem[i] = NaN; }
    }
    if (!anyValid) { p.reason = 'No time with complete data'; return p; }
    s.t = grid; s.step = step;
    p.s = s;
    p.weight = weight;
    p.replicas = replicas;
    p.limCpu = hasLimCpu ? limCpu : null;
    p.limMem = hasLimMem ? limMem : null;
    p.wasteCpu = new Float64Array(n);
    p.wasteMem = new Float64Array(n);
    p.status = p.containers.some(function (c) { return c.excluded; }) ? 'partial' : 'ok';
    if (p.status === 'partial') p.reason = 'Some containers lack data and are left out';
    return p;
  }

  // OOM kills across a workload's containers, summed by time: {t, n}, sorted.
  function oomKills(parts) {
    var byTime = {};
    parts.forEach(function (c) {
      var s = c.series[M.oom];
      if (!s) return;
      for (var i = 0; i < s.t.length; i++) if (s.v[i] > 0) byTime[s.t[i]] = (byTime[s.t[i]] || 0) + s.v[i];
    });
    var t = Object.keys(byTime).map(Number).sort(function (a, b) { return a - b; });
    return { t: Float64Array.from(t), n: Float64Array.from(t.map(function (x) { return byTime[x]; })) };
  }

  // First index whose time is at or after t, in a sorted time array.
  function lowerBound(t, x) {
    var lo = 0, hi = t.length;
    while (lo < hi) { var mid = (lo + hi) >> 1; if (t[mid] < x) lo = mid + 1; else hi = mid; }
    return lo;
  }

  // First index whose time is after t.
  function upperBound(t, x) {
    var lo = 0, hi = t.length;
    while (lo < hi) { var mid = (lo + hi) >> 1; if (t[mid] <= x) lo = mid + 1; else hi = mid; }
    return lo;
  }

  // The workload's figures under the given levers. The target, spend and
  // waste arrays belong to the prepared workload and are overwritten by the
  // next call.
  function computeWorkload(p, rates, lv) {
    var res = {
      id: p.id, cluster: p.cluster, ns: p.ns, kind: p.kind, name: p.name, containers: p.containers, notes: p.notes,
      status: p.status, reason: p.reason, series: null,
      annualSpend: 0, annualSavings: 0, savingsPct: 0, wasteCores: 0, wasteGiB: 0, reqCores: 0, reqGiB: 0, useCores: 0, useGiB: 0
    };
    if (!p.s) return res;
    var s = p.s, n = s.t.length, weight = p.weight, lo = 0, hi = n;
    if (lv.range) {
      lo = lowerBound(s.t, lv.range[0]);
      hi = upperBound(s.t, lv.range[1]);
      var any = false;
      for (var j = lo; j < hi && !any; j++) any = isFinite(s.reqCpu[j]);
      if (!any) { res.status = 'excluded'; res.reason = 'No data in the selected time range'; return res; }
    }
    if (!p.peaks || p.peaks.window !== lv.peakWindow) {
      p.peaks = { window: lv.peakWindow, cpu: rollingMax(s.t, s.peakCpu, lv.peakWindow), mem: rollingMax(s.t, s.peakMem, lv.peakWindow) };
    }
    var pc = p.peaks.cpu, pm = p.peaks.mem, h = lv.headroom;
    for (var k = 0; k < n; k++) {
      s.tgtCpu[k] = pc[k] * h;
      s.tgtMem[k] = pm[k] * h;
      s.cur[k] = Math.max(s.reqCpu[k] * rates.perCore, s.reqMem[k] * rates.perGiB);
      s.opt[k] = Math.max(Math.min(s.reqCpu[k], s.tgtCpu[k]) * rates.perCore, Math.min(s.reqMem[k], s.tgtMem[k]) * rates.perGiB);
      s.useCost[k] = Math.max(s.useCpu[k] * rates.perCore, s.useMem[k] * rates.perGiB);
      p.wasteCpu[k] = Math.max(0, s.reqCpu[k] - s.tgtCpu[k]);
      p.wasteMem[k] = Math.max(0, s.reqMem[k] - s.tgtMem[k]);
    }
    res.series = lv.range ? clip(s, lv.range) : s;
    res.reqCores = mean(s.reqCpu, weight, lo, hi);
    res.reqGiB = mean(s.reqMem, weight, lo, hi);
    res.useCores = mean(s.useCpu, weight, lo, hi);
    res.useGiB = mean(s.useMem, weight, lo, hi);
    var cur = mean(s.cur, weight, lo, hi);
    res.annualSpend = cur * HOURS_PER_YEAR;
    res.annualSavings = (cur - mean(s.opt, weight, lo, hi)) * HOURS_PER_YEAR;
    res.savingsPct = res.annualSpend > 0 ? res.annualSavings / res.annualSpend : 0;
    res.wasteCores = mean(p.wasteCpu, weight, lo, hi);
    res.wasteGiB = mean(p.wasteMem, weight, lo, hi);
    return res;
  }

  function shortMetric(name) {
    if (name === M.cpuUse) return 'CPU usage';
    if (name === M.memUse) return 'memory usage';
    if (name === M.cpuReq) return 'CPU request';
    if (name === M.memReq) return 'memory request';
    return name;
  }

  // Sum workload series onto one grid. Each workload adds its mean to a
  // bucket, except for peaks, where it adds its highest point, and adds
  // nothing outside its first and last point. A bucket inside that span where
  // the workload has no value takes the value of its nearest bucket that has
  // one. The bucket counts only when the workloads filled this way make up at
  // most FILL_SHARE of the spend of the workloads in it; otherwise every field
  // is NaN. Buckets at either end that do not count are dropped.
  var PEAKS = ['peakCpu', 'peakMem'];
  var FILL_SHARE = 0.05;
  function combine(list) {
    var step = 0, t0 = Infinity, t1 = -Infinity;
    list.forEach(function (w) {
      step = Math.max(step, w.series.step);
      t0 = Math.min(t0, w.series.t[0]);
      t1 = Math.max(t1, w.series.t[w.series.t.length - 1]);
    });
    if (!(step > 0)) step = 1;
    var nb = Math.floor((t1 - t0) / step) + 1, nf = FIELDS.length;
    var out = { t: new Float64Array(nb), step: step };
    for (var b = 0; b < nb; b++) out.t[b] = t0 + b * step;
    var outs = FIELDS.map(function (f) { return (out[f] = new Float64Array(nb)); });
    var peak = FIELDS.map(function (f) { return PEAKS.indexOf(f) >= 0; });
    // Spend of the workloads in each bucket, of those filled there, and how
    // many have a value there.
    var inSpend = new Float64Array(nb), fillSpend = new Float64Array(nb), real = new Float64Array(nb);
    // One scratch row per field, reset over each workload's own buckets.
    var sum = FIELDS.map(function () { return new Float64Array(nb); }), cnt = new Float64Array(nb), near = new Int32Array(nb);
    var bin = new Int32Array(0);
    list.forEach(function (w) {
      var s = w.series, n = s.t.length, i, k;
      var first = Math.floor((s.t[0] - t0) / step), last = Math.floor((s.t[n - 1] - t0) / step);
      var spend = w.annualSpend > 0 ? w.annualSpend : 0;
      if (bin.length < n) bin = new Int32Array(n);
      // The bucket of each valid point, or -1.
      for (i = 0; i < n; i++) bin[i] = isFinite(s.cur[i]) ? Math.floor((s.t[i] - t0) / step) : -1;
      for (k = first; k <= last; k++) cnt[k] = 0;
      for (i = 0; i < n; i++) if (bin[i] >= 0) cnt[bin[i]]++;
      // The nearest bucket with a value, the earlier one on a tie.
      var prev = -1;
      for (k = first; k <= last; k++) { if (cnt[k]) prev = k; near[k] = prev; }
      var next = -1;
      for (k = last; k >= first; k--) {
        if (cnt[k]) next = k;
        if (next >= 0 && (near[k] < 0 || next - k < k - near[k])) near[k] = next;
      }
      for (k = first; k <= last; k++) {
        inSpend[k] += spend;
        if (cnt[k]) real[k]++;
        else fillSpend[k] += spend;
      }
      for (var f = 0; f < nf; f++) {
        var row = sum[f], src = s[FIELDS[f]], dst = outs[f];
        for (k = first; k <= last; k++) row[k] = peak[f] ? -Infinity : 0;
        if (peak[f]) { for (i = 0; i < n; i++) if (bin[i] >= 0) row[bin[i]] = Math.max(row[bin[i]], src[i]); }
        else { for (i = 0; i < n; i++) if (bin[i] >= 0) row[bin[i]] += src[i]; }
        for (k = first; k <= last; k++) if (cnt[k]) row[k] = peak[f] ? row[k] : row[k] / cnt[k];
        for (k = first; k <= last; k++) if (near[k] >= 0) dst[k] += row[near[k]];
      }
    });
    var lo = nb, hi = -1;
    for (var q = 0; q < nb; q++) {
      if (real[q] && fillSpend[q] <= inSpend[q] * FILL_SHARE) { lo = Math.min(lo, q); hi = q; continue; }
      for (var g = 0; g < nf; g++) outs[g][q] = NaN;
    }
    if (hi < 0) return out;
    out.t = out.t.subarray(lo, hi + 1);
    FIELDS.forEach(function (f) { out[f] = out[f].subarray(lo, hi + 1); });
    return out;
  }

  // How each thoras.source value is named on the page.
  var SOURCE_LABELS = { 'kubectl-snapshot': 'live polling' };

  // Fidelity notes the page explains; others are ignored, as the contract asks.
  var NOTES = {
    usage_split_from_pod_by_requests: {
      workload: 'Usage split from pod totals by requests',
      summary: function (n) {
        return 'Container usage in ' + n + (n === 1 ? ' workload' : ' workloads') +
          ' is the pod total split between containers by their requests, because the source has no per-container usage.';
      }
    }
  };

  function workloadNotes(parts) {
    var out = [];
    parts.forEach(function (c) {
      c.notes.forEach(function (n) { if (NOTES[n] && out.indexOf(n) < 0) out.push(n); });
    });
    return out;
  }

  function noteSummaries(workloads) {
    var counts = {}, order = [];
    workloads.forEach(function (w) {
      w.notes.forEach(function (n) {
        if (!counts[n]) { counts[n] = 0; order.push(n); }
        counts[n]++;
      });
    });
    return order.map(function (n) { return NOTES[n].summary(counts[n]); });
  }

  function fidelity(containers) {
    var tier = 1, step = 0, sourceStep = 0, t0 = Infinity, t1 = -Infinity, sources = [];
    containers.forEach(function (c) {
      tier = Math.max(tier, c.tier);
      step = Math.max(step, c.coarseStep);
      sourceStep = Math.max(sourceStep, c.sourceStep);
      var src = SOURCE_LABELS[c.source] || c.source;
      if (src && sources.indexOf(src) < 0) sources.push(src);
      [M.cpuUse, M.memUse].forEach(function (n) {
        var s = c.series[n];
        if (s && s.t.length) { t0 = Math.min(t0, s.t[0]); t1 = Math.max(t1, s.t[s.t.length - 1] + stepAt(c.spans, s.t[s.t.length - 1], c.step)); }
      });
    });
    var span = isFinite(t0) ? t1 - t0 : 0;
    var points = step > 0 ? span / step : 0;
    if (!(sourceStep < step)) sourceStep = 0;
    return {
      tier: tier, stepSeconds: step, sourceStepSeconds: sourceStep, spanSeconds: span, snapshot: points <= 1,
      start: isFinite(t0) ? t0 : null, end: isFinite(t0) ? t1 : null,
      source: sources.join(', '), label: fidelityLabel(step, span, points <= 1, sourceStep)
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
    if (seconds < 60) return seconds + (seconds === 1 ? ' second' : ' seconds');
    if (seconds < 3570) {
      var m = Math.round(seconds / 60);
      return m + (m === 1 ? ' minute' : ' minutes');
    }
    var hours = Math.round(seconds / 360) / 10;
    return hours + (hours === 1 ? ' hour' : ' hours');
  }

  // A compact file keeps the peaks of finer data, so it names both steps.
  function fidelityLabel(step, span, snapshot, sourceStep) {
    if (snapshot) return 'Snapshot only';
    if (sourceStep > 0 && sourceStep < step) {
      return formatSpan(span) + ' at ' + formatStep(sourceStep) + ' resolution, kept as ' + formatStep(step) + ' peaks and means';
    }
    return formatSpan(span) + ' at ' + formatStep(step) + ' resolution';
  }

  // Fidelity of several clusters together: the worst tier, the coarsest
  // step, the earliest start to the latest end.
  function mergeFidelity(list) {
    if (list.length === 1) return list[0];
    var tier = 1, step = 0, sourceStep = 0, t0 = Infinity, t1 = -Infinity, sources = [], notes = [];
    list.forEach(function (f) {
      tier = Math.max(tier, f.tier);
      step = Math.max(step, f.stepSeconds);
      sourceStep = Math.max(sourceStep, f.sourceStepSeconds);
      if (f.start !== null) { t0 = Math.min(t0, f.start); t1 = Math.max(t1, f.end); }
      f.source.split(', ').forEach(function (x) { if (x && sources.indexOf(x) < 0) sources.push(x); });
      (f.notes || []).forEach(function (n) { if (notes.indexOf(n) < 0) notes.push(n); });
    });
    var span = isFinite(t0) ? t1 - t0 : 0;
    var points = step > 0 ? span / step : 0;
    if (!(sourceStep < step)) sourceStep = 0;
    return {
      tier: tier, stepSeconds: step, sourceStepSeconds: sourceStep, spanSeconds: span, snapshot: points <= 1,
      start: isFinite(t0) ? t0 : null, end: isFinite(t0) ? t1 : null,
      source: sources.join(', '), label: fidelityLabel(step, span, points <= 1, sourceStep), notes: notes
    };
  }

  // Rates for a fleet's page-wide figures: the clusters' priced nodes pooled.
  // Each workload is priced with its own cluster's rates.
  function fleetPricing(clusters, nodes) {
    var p = pricing(nodes);
    var bases = [];
    clusters.forEach(function (c) { if (bases.indexOf(c.pricing.basis) < 0) bases.push(c.pricing.basis); });
    if (bases.length > 1) p.basis = 'each cluster priced from its own nodes';
    else p.basis = bases[0];
    return p;
  }

  // Reads a file once: parses it, prices its nodes and lines up each
  // workload's series. compute() turns the result into figures.
  //
  // In a file with several clusters each cluster is priced from its own
  // nodes, workload ids start with the cluster's name, and namespaces are
  // named cluster/namespace, since two clusters can have the same ones.
  //   opts.dropped  [{cluster, kept, dropped}]: clusters read from more than
  //                 one file, and which file's data is used (files.combine)
  function prepare(doc, opts) {
    var data = parse(doc);
    var multi = data.clusters.length > 1;
    var byWorkload = {}, order = [];
    data.containers.forEach(function (c) {
      var key = c.cluster + '\u0000' + c.workloadId;
      if (!byWorkload[key]) { byWorkload[key] = []; order.push(key); }
      byWorkload[key].push(c);
    });
    var workloads = order.map(function (key) {
      var first = byWorkload[key][0];
      var info = { ns: first.ns, kind: first.kind, name: first.workload, workload: data.workloads[key] };
      var p = prepareWorkload(multi ? first.cluster + '/' + first.workloadId : first.workloadId, info, byWorkload[key]);
      p.cluster = first.cluster;
      if (multi) p.ns = first.cluster + '/' + first.ns;
      p.notes = workloadNotes(byWorkload[key]);
      return p;
    });
    var namespaces = [];
    workloads.forEach(function (w) { if (namespaces.indexOf(w.ns) < 0) namespaces.push(w.ns); });
    var clusters = data.clusters.map(function (name) {
      var mine = function (r) { return r.cluster === name; };
      var own = workloads.filter(mine);
      var cs = data.containers.filter(mine);
      var f = cs.length ? fidelity(cs) : fidelity([]);
      f.notes = noteSummaries(own);
      var nodes = data.nodes.filter(mine);
      return { name: name, pricing: pricing(nodes), nodes: nodes, fidelity: f, workloads: own.length };
    });
    var fid = multi ? mergeFidelity(clusters.map(function (c) { return c.fidelity; })) : fidelity(data.containers);
    fid.notes = noteSummaries(workloads);
    return {
      cluster: multi ? clusters.length + ' clusters' : data.cluster, contract: data.contract, fidelity: fid,
      pricing: multi ? fleetPricing(clusters, data.nodes) : clusters[0].pricing, nodes: data.nodes,
      namespaces: namespaces.sort(), workloads: workloads,
      clusters: clusters, failed: data.failed, dropped: (opts && opts.dropped) || [], multi: multi
    };
  }

  var DEFAULT_LEVERS = { rates: null, discount: 0, headroom: TARGET_HEADROOM, peakWindow: PEAK_WINDOW_SECONDS, namespaces: null, clusters: null, range: null };

  function fmtRate(v) { return '$' + Number(v.toPrecision(4)); }

  function discounted(rates, discount) {
    if (!(discount > 0)) return rates;
    return Object.assign({}, rates, {
      perCore: rates.perCore * (1 - discount), perGiB: rates.perGiB * (1 - discount), discount: discount,
      basis: rates.basis + '; ' + Number((discount * 100).toPrecision(4)) + '% discount applied'
    });
  }

  // The one place totals are computed. With DEFAULT_LEVERS it matches
  // internal/savings, which the parity tests check.
  //   rates       {perCore, perGiB} in USD per hour, or null to price from the nodes
  //   discount    fraction taken off the rates (0)
  //   headroom    target as a multiple of the peak (1.1)
  //   peakWindow  seconds the rolling peak looks back (900)
  //   namespaces  namespaces to count, or null for all
  //   clusters    clusters to count, or null for all
  //   range       [from, to] in Unix seconds, or null for the whole file
  function compute(data, levers) {
    var lv = Object.assign({}, DEFAULT_LEVERS, levers);
    var clusters = data.clusters || [{ name: data.cluster, pricing: data.pricing, nodes: data.nodes, fidelity: data.fidelity }];
    var inScope = lv.clusters ? clusters.filter(function (c) { return lv.clusters.indexOf(c.name) >= 0; }) : clusters;
    if (!inScope.length) inScope = clusters;
    var one = inScope.length === 1;
    var base = one ? inScope[0].pricing : data.pricing;
    var custom = null;
    if (lv.rates) {
      custom = Object.assign({}, base, {
        perCore: lv.rates.perCore, perGiB: lv.rates.perGiB, custom: true,
        basis: fmtRate(lv.rates.perCore) + ' per core-hour and ' + fmtRate(lv.rates.perGiB) + ' per GiB-hour'
      });
    }
    var rates = discounted(custom || base, lv.discount);
    var own = {};
    inScope.forEach(function (c) { own[c.name] = discounted(custom || c.pricing, lv.discount); });
    var scope = null;
    if (lv.namespaces) { scope = {}; lv.namespaces.forEach(function (n) { scope[n] = true; }); }
    var workloads = [];
    data.workloads.forEach(function (p) {
      var r = data.multi ? own[p.cluster] : rates;
      if (r && (!scope || scope[p.ns])) workloads.push(computeWorkload(p, r, lv));
    });
    var counted = workloads.filter(function (w) { return w.series; });
    var totals = { spend: 0, savings: 0, cores: 0, gib: 0, reqCores: 0, reqGiB: 0, useCores: 0, useGiB: 0, workloads: workloads.length, counted: counted.length };
    var perCluster = {};
    inScope.forEach(function (c) { perCluster[c.name] = { name: c.name, spend: 0, savings: 0, workloads: 0, counted: 0, fidelity: c.fidelity }; });
    workloads.forEach(function (w) {
      var pc = perCluster[w.cluster];
      if (pc) pc.workloads++;
      if (!w.series) return;
      totals.spend += w.annualSpend; totals.savings += w.annualSavings;
      totals.cores += w.wasteCores; totals.gib += w.wasteGiB;
      totals.reqCores += w.reqCores; totals.reqGiB += w.reqGiB;
      totals.useCores += w.useCores; totals.useGiB += w.useGiB;
      if (pc) { pc.counted++; pc.spend += w.annualSpend; pc.savings += w.annualSavings; }
    });
    var series = counted.length ? combine(counted) : null;
    if (series && lv.range) series = clip(series, lv.range);
    var byCluster = inScope.map(function (c) { return perCluster[c.name]; })
      .sort(function (a, b) { return b.savings - a.savings || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0); });
    return {
      cluster: one ? (data.multi ? inScope[0].name : data.cluster) : inScope.length + ' clusters',
      fidelity: data.multi && one ? inScope[0].fidelity : data.fidelity, pricing: rates, basePricing: base, namespaces: data.namespaces,
      nodes: one ? inScope[0].nodes : data.nodes, clusters: byCluster, fleet: data.multi && byCluster.filter(function (c) { return c.counted; }).length > 1,
      levers: lv, workloads: workloads, totals: totals, series: series
    };
  }

  function clip(series, range) {
    var a = lowerBound(series.t, range[0]), b = upperBound(series.t, range[1]);
    var out = { step: series.step, t: series.t.subarray(a, b) };
    FIELDS.forEach(function (f) { out[f] = series[f].subarray(a, b); });
    return out;
  }

  function analyze(doc) {
    return compute(prepare(doc));
  }

  var api = {
    analyze: analyze, prepare: prepare, mergeFidelity: mergeFidelity, priceNode: priceNode, priceTable: PRICES, cloudNames: CLOUD_NAMES, compute: compute, defaultLevers: DEFAULT_LEVERS, parse: parse, pricing: pricing, stepAt: stepAt, resample: resample, rollingMax: rollingMax,
    lowerBound: lowerBound, upperBound: upperBound, mean: mean,
    fidelityLabel: fidelityLabel, formatSpan: formatSpan, formatStep: formatStep, notes: NOTES, constants: { GIB: GIB, HOURS_PER_YEAR: HOURS_PER_YEAR, PEAK_WINDOW_SECONDS: PEAK_WINDOW_SECONDS, TARGET_HEADROOM: TARGET_HEADROOM, DEFAULT_NODE: DEFAULT_NODE }
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).model = api;
})();
