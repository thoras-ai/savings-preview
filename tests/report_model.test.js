'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');

const GIB = 1 << 30;
const S = (k, v) => ({ key: k, value: { stringValue: v } });
const I = (k, v) => ({ key: k, value: { intValue: String(v) } });

function series(name, unit, values, step, opts = {}) {
  const pts = [];
  values.forEach((v, i) => {
    if (v === null) return;
    const p = { timeUnixNano: String((1000000 + i * step) * 1e9) };
    if (opts.int) p.asInt = String(v); else p.asDouble = v;
    pts.push(p);
  });
  return { name, unit, [opts.gauge ? 'gauge' : 'sum']: { dataPoints: pts } };
}

function doc({ cpu, mem, cpuReq = 2, memReq = 4, step = 300, replicas = 1, nodes = [], missing = [] }) {
  const base = [S('k8s.cluster.name', 'c'), S('thoras.source', 'test'), I('thoras.step_seconds', step), I('thoras.fidelity_tier', 2)];
  const wl = [S('k8s.namespace.name', 'ns'), S('k8s.deployment.name', 'api')];
  const n = cpu.length;
  const metrics = [
    series('container.cpu.usage', '{cpu}', cpu, step, { gauge: true }),
    series('container.memory.working_set', 'By', mem.map(v => v === null ? null : v * GIB), step, { int: true }),
    series('k8s.container.cpu.request.desired', '{cpu}', cpu.map(() => cpuReq), step),
    series('k8s.container.memory.request.desired', 'By', cpu.map(() => memReq * GIB), step, { int: true })
  ].filter(m => !missing.includes(m.name));
  const attrs = base.concat(wl, [S('k8s.container.name', 'app')]);
  if (missing.length) attrs.push({ key: 'thoras.missing_metrics', value: { arrayValue: { values: missing.map(m => ({ stringValue: m })) } } });
  return {
    resourceMetrics: [
      { resource: { attributes: base.concat(wl) }, scopeMetrics: [{ metrics: [series('k8s.deployment.pod.desired', '{pod}', cpu.map(() => replicas), step, { int: true })] }] },
      { resource: { attributes: attrs }, scopeMetrics: [{ metrics }] }
    ].concat(nodes)
  };
}

test('waste is request minus 1.1 times the 15-minute peak', () => {
  // 1-minute steps: usage peaks at 1.0 inside every 15-minute window.
  const cpu = Array.from({ length: 60 }, (_, i) => (i % 15 === 0 ? 1 : 0.2));
  const mem = cpu.map(() => 1);
  const r = model.analyze(doc({ cpu, mem, step: 60, replicas: 2 }));
  const w = r.workloads[0];
  assert.strictEqual(w.status, 'ok');
  // Request 2 cpu x 2 replicas = 4; target = 1.1 x (1.0 x 2) = 2.2; waste 1.8. Memory 8 vs 2.2: waste 5.8.
  assert.ok(Math.abs(w.wasteCores - 1.8) < 0.1, String(w.wasteCores));
  assert.ok(Math.abs(w.wasteGiB - 5.8) < 0.1, String(w.wasteGiB));
});

test('default node pricing applies when the file has no nodes', () => {
  const r = model.analyze(doc({ cpu: [0.5, 0.5], mem: [1, 1], cpuReq: 4, memReq: 1 }));
  assert.strictEqual(r.pricing.priced, 0);
  // CPU cost 4 x 0.048 per hour is larger than memory cost 1 x 0.012.
  assert.ok(Math.abs(r.totals.spend - 4 * 0.048 * 8760) < 1);
});

test('a missing required series excludes the workload and is explained', () => {
  const r = model.analyze(doc({ cpu: [1, 1, 1], mem: [1, 1, 1], missing: ['container.memory.working_set'] }));
  const w = r.workloads[0];
  assert.strictEqual(w.status, 'excluded');
  assert.strictEqual(w.series, null);
  assert.match(w.reason, /memory usage/);
  assert.strictEqual(r.series, null);
  assert.strictEqual(r.totals.counted, 0);
});

test('gaps are left out, not filled', () => {
  const cpu = [1, 1, null, 1, 1], mem = [1, 1, 1, 1, 1];
  const r = model.analyze(doc({ cpu, mem }));
  const s = r.workloads[0].series;
  assert.strictEqual(s.t.length, 5);
  assert.ok(Number.isNaN(s.cur[2]));
  assert.ok(Number.isFinite(s.cur[3]));
  assert.ok(Number.isNaN(r.series.cur[2]));
});

test('a single point is labeled a snapshot', () => {
  const r = model.analyze(doc({ cpu: [1], mem: [1] }));
  assert.strictEqual(r.fidelity.label, 'Snapshot only');
  assert.ok(r.totals.spend > 0);
});

test('fidelity label states span and resolution', () => {
  assert.strictEqual(model.fidelityLabel(300, 14 * 86400, false), '14 days at 5-minute resolution');
  assert.strictEqual(model.fidelityLabel(60, 7 * 86400, false), '7 days at 1-minute resolution');
});

test('a compact file names the resolution it was built from', () => {
  assert.strictEqual(model.fidelityLabel(900, 14 * 86400, false, 60), '14 days at 1-minute resolution, kept as 15-minute peaks and means');
  assert.strictEqual(model.fidelityLabel(900, 14 * 86400, false, 0), '14 days at 15-minute resolution');
});

test('held series stay current until their next point, and the last one for a step', () => {
  const t = Float64Array.from([100, 400, 500]), v = Float64Array.from([1, 4, 5]);
  const grid = [50, 100, 300, 399, 400, 450, 500, 599, 600];
  const held = Array.from(model.resample({ t, v }, grid, 100, null, true));
  assert.deepStrictEqual(held, [NaN, 1, 1, 1, 4, 4, 5, 5, NaN]);
  const dense = Array.from(model.resample({ t, v }, grid, 100, null, false));
  assert.deepStrictEqual(dense, [NaN, 1, NaN, NaN, 4, 4, 5, 5, NaN]);
});

test('a compact file with held requests prices like its dense form', () => {
  const cpu = Array.from({ length: 40 }, (_, i) => 0.2 + (i % 7) / 20);
  const mem = cpu.map(() => 1);
  const dense = doc({ cpu, mem, step: 900, cpuReq: 1, memReq: 2, replicas: 3 });
  const compact = JSON.parse(JSON.stringify(dense));
  compact.resourceMetrics.forEach(rm => {
    const levels = [];
    rm.scopeMetrics[0].metrics.forEach(m => {
      if (!/request|pod\.desired/.test(m.name)) return;
      const pts = m.sum.dataPoints;
      m.sum.dataPoints = [pts[0], pts[pts.length - 1]];
      levels.push({ stringValue: m.name });
    });
    rm.resource.attributes.push(I('thoras.source_step_seconds', 60), { key: 'thoras.held_metrics', value: { arrayValue: { values: levels } } });
  });
  const a = model.analyze(dense), b = model.analyze(compact);
  for (const k of ['spend', 'savings', 'cores', 'gib', 'reqCores', 'reqGiB']) assert.ok(Math.abs(a.totals[k] - b.totals[k]) < 1e-9, k);
  assert.strictEqual(b.fidelity.sourceStepSeconds, 60);
  assert.match(b.fidelity.label, /at 1-minute resolution, kept as 15-minute peaks/);
});

test('rolling max covers only the trailing window', () => {
  const t = [0, 300, 600, 900, 1200], v = [5, 1, 1, 1, 1];
  const out = Array.from(model.rollingMax(t, v, 900));
  assert.deepStrictEqual(out, [5, 5, 5, 1, 1]);
});

test('the fixture produces the expected workload states', () => {
  const file = path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json');
  const r = model.analyze(JSON.parse(fs.readFileSync(file, 'utf8')));
  const byName = Object.fromEntries(r.workloads.map(w => [w.name, w]));
  assert.strictEqual(r.workloads.length, 4);
  assert.strictEqual(byName['report-worker'].status, 'excluded');
  assert.ok(byName['checkout-api'].annualSavings > byName['ledger-db'].annualSavings);
  assert.strictEqual(r.fidelity.label, '14 days at 5-minute resolution');
  const sum = r.workloads.reduce((a, w) => a + w.annualSavings, 0);
  assert.ok(Math.abs(sum - r.totals.savings) < 1e-6);
});

test('files that are not reports are rejected with a clear message', () => {
  assert.throws(() => model.analyze({}), /not a Savings Preview file/);
  const earlier = fixture();
  earlier.resourceMetrics[0].scopeMetrics[0].scope.version = '1.2.0';
  assert.throws(() => model.analyze(earlier), /contract version 1\.2\.0 and this page reads only 1\.0\.0/);
});

function multi(workloads, nodes = []) {
  const rms = [];
  workloads.forEach(({ name, replicas, containers, cpu }) => {
    const base = [S('k8s.cluster.name', 'c'), I('thoras.step_seconds', 300), I('thoras.fidelity_tier', 2), S('k8s.namespace.name', 'ns'), S('k8s.deployment.name', name)];
    const n = cpu.length;
    const first = containers[0].first || 0;
    rms.push({ resource: { attributes: base }, scopeMetrics: [{ metrics: [series('k8s.deployment.pod.desired', '{pod}', Array(n).fill(replicas), 300, { int: true })] }] });
    containers.forEach(c => {
      const off = new Set(c.off || []);
      const v = (x) => Array.from({ length: n }, (_, i) => (i < first || off.has(i) ? null : x));
      const metrics = [
        series('container.cpu.usage', '{cpu}', v(c.use), 300, { gauge: true }),
        series('container.memory.working_set', 'By', v(1 * GIB), 300, { int: true }),
        series('k8s.container.cpu.request.desired', '{cpu}', v(c.req), 300),
        series('k8s.container.memory.request.desired', 'By', v(GIB), 300, { int: true })
      ].filter(m => !(c.drop || []).includes(m.name));
      rms.push({ resource: { attributes: base.concat([S('k8s.container.name', c.name)]) }, scopeMetrics: [{ metrics }] });
    });
  });
  return { resourceMetrics: rms.concat(nodes) };
}

test('a workload that starts late does not blank the cluster series', () => {
  const cpu = Array(100).fill(0);
  const r = model.analyze(multi([
    { name: 'old', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1 }] },
    { name: 'new', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1, first: 90 }] }
  ]));
  const valid = Array.from(r.series.cur).filter(Number.isFinite).length;
  assert.strictEqual(valid, 100);
  assert.ok(r.series.reqCpu[0] < r.series.reqCpu[99]);
});

test('a gap inside a workload range still drops the cluster bucket', () => {
  const r = model.analyze(doc({ cpu: [1, 1, null, 1, 1], mem: [1, 1, 1, 1, 1] }));
  assert.ok(Number.isNaN(r.series.cur[2]));
});

test('a container without usage at a step is not running then and adds no usage or request', () => {
  const cpu = Array(10).fill(0);
  const r = model.analyze(multi([
    { name: 'w', replicas: 2, cpu, containers: [{ name: 'app', use: 0.5, req: 1 }, { name: 'check', use: 0.25, req: 3, off: [0, 1, 2, 3, 4, 6, 7, 8, 9] }] }
  ]));
  const w = r.workloads[0];
  assert.strictEqual(w.status, 'ok');
  assert.ok(Array.from(w.series.cur).every(Number.isFinite));
  assert.strictEqual(w.series.reqCpu[0], 2);
  assert.strictEqual(w.series.reqCpu[5], 8);
  assert.strictEqual(w.series.useCpu[5], 1.5);
  assert.ok(Math.abs(w.reqCores - 2.6) < 1e-9);
});

test('a workload is unknown at a step when none of its containers has usage', () => {
  const cpu = Array(10).fill(0);
  const r = model.analyze(multi([
    { name: 'w', replicas: 1, cpu, containers: [{ name: 'a', use: 0.5, req: 1, off: [4] }, { name: 'b', use: 0.5, req: 1, off: [4, 5] }] }
  ]));
  const s = r.workloads[0].series;
  assert.strictEqual(s.t.length, 9);
  assert.strictEqual(s.reqCpu[4], 1);
  assert.strictEqual(s.reqCpu[5], 2);
  assert.ok(Number.isNaN(r.series.cur[4]));
});

test('a container with only one usage value at a step is not running then', () => {
  const cpu = Array(4).fill(0);
  const doc2 = multi([{ name: 'w', replicas: 1, cpu, containers: [{ name: 'a', use: 0.5, req: 1 }, { name: 'b', use: 0.5, req: 1 }] }]);
  // Container b loses its CPU point at step 1 but keeps memory.
  const b = doc2.resourceMetrics[2].scopeMetrics[0].metrics.find(m => m.name === 'container.cpu.usage');
  b.gauge.dataPoints.splice(1, 1);
  const s = model.analyze(doc2).workloads[0].series;
  assert.strictEqual(s.reqCpu[0], 2);
  assert.strictEqual(s.reqCpu[1], 1);
  assert.strictEqual(s.reqCpu[2], 2);
});

test('a small workload with a gap is filled from its nearest bucket, a large one drops the bucket', () => {
  const cpu = Array(10).fill(0);
  const small = model.analyze(multi([
    { name: 'big', replicas: 1, cpu, containers: [{ name: 'a', use: 1, req: 40 }] },
    { name: 'small', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1, off: [3, 4, 5, 6] }] }
  ]));
  assert.ok(Array.from(small.series.cur).every(Number.isFinite));
  assert.strictEqual(small.series.reqCpu[3], 41);
  const large = model.analyze(multi([
    { name: 'big', replicas: 1, cpu, containers: [{ name: 'a', use: 1, req: 40, off: [3] }] },
    { name: 'small', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1 }] }
  ]));
  assert.ok(Number.isNaN(large.series.cur[3]));
  assert.strictEqual(large.series.reqCpu[4], 41);
});

test('leading buckets where no workload has data are dropped from the cluster series', () => {
  const cpu = Array(10).fill(0);
  const d = multi([
    { name: 'gappy', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1, off: [0, 1, 2] }] },
    { name: 'late', replicas: 1, cpu, containers: [{ name: 'a', use: 0.1, req: 1, first: 3 }] }
  ]);
  // The gappy workload keeps CPU points from the start, as a store with
  // shorter memory history gives.
  const cpuSeries = d.resourceMetrics[1].scopeMetrics[0].metrics.find(m => m.name === 'container.cpu.usage');
  cpuSeries.gauge.dataPoints = [0, 1, 2].map(i => ({ timeUnixNano: String((1000000 + i * 300) * 1e9), asDouble: 0.1 })).concat(cpuSeries.gauge.dataPoints);
  const r = model.analyze(d);
  assert.strictEqual(r.workloads[0].series.t.length, 10);
  assert.strictEqual(r.series.t.length, 7);
  assert.strictEqual(r.series.t[0], 1000000 + 3 * 300);
  assert.ok(Array.from(r.series.cur).every(Number.isFinite));
});

test('intermittent containers and late series do not blank the cluster chart', () => {
  const file = path.join(__dirname, '..', 'fixtures', 'intermittent.otlp.json');
  const r = model.analyze(JSON.parse(fs.readFileSync(file, 'utf8')));
  assert.strictEqual(r.totals.counted, 5);
  assert.strictEqual(r.series.t.length, 192);
  assert.ok(Array.from(r.series.cur).every(Number.isFinite));
  const lg = r.workloads.find(w => w.name === 'loadgenerator');
  assert.strictEqual(lg.status, 'partial');
  assert.deepStrictEqual(lg.containers.filter(c => c.excluded).map(c => c.name), ['istio-init']);
  assert.ok(Array.from(lg.series.cur).every(Number.isFinite));
  // main, istio-proxy and test-sidecar request 0.45 cores; the check adds 0.1 only where it has both usage values.
  assert.ok(Math.abs(lg.series.reqCpu[10] - 0.45) < 1e-9);
  assert.ok(Math.abs(lg.series.reqCpu[20] - 0.45) < 1e-9);
  assert.ok(Math.abs(lg.series.reqCpu[150] - 0.55) < 1e-9);
  // The monitoring workloads have no memory or replicas for six hours; the
  // chart fills them from their first value, since they are a small share.
  const prom = r.workloads.find(w => w.name === 'prometheus-server');
  assert.ok(Number.isNaN(prom.series.cur[23]) && Number.isFinite(prom.series.cur[24]));
  assert.ok(Math.abs(r.series.reqCpu[0] - r.series.reqCpu[24]) < 1e-9);
});

test('totals for the committed fixtures keep their golden values', () => {
  const want = {
    'savings-preview.otlp.json': { spend: 3375.052799999748, savings: 1210.4480350292247, reqCores: 14, reqGiB: 44, counted: 3 },
    'minikube-poll.otlp.json': { spend: 1595.0728125, savings: 1468.3183583557436, reqCores: 3.236, reqGiB: 8.78125, counted: 21 }
  };
  Object.entries(want).forEach(([name, w]) => {
    const r = model.analyze(JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', name), 'utf8')));
    Object.entries(w).forEach(([k, v]) => assert.ok(Math.abs(r.totals[k] - v) <= 1e-9 * Math.max(1, Math.abs(v)), name + ' ' + k + ': ' + r.totals[k]));
  });
});

test('totals multiply per-pod values by replicas and sum containers', () => {
  const cpu = Array(10).fill(0);
  const r = model.analyze(multi([
    { name: 'w', replicas: 3, cpu, containers: [{ name: 'a', use: 0.5, req: 2 }, { name: 'b', use: 0.25, req: 1 }] }
  ]));
  const w = r.workloads[0];
  assert.ok(Math.abs(w.reqCores - 9) < 1e-9);
  assert.ok(Math.abs(w.series.useCpu[5] - 2.25) < 1e-9);
});

test('a partial workload counts only its complete containers', () => {
  const cpu = Array(10).fill(0);
  const r = model.analyze(multi([
    { name: 'w', replicas: 1, cpu, containers: [{ name: 'a', use: 0.5, req: 2 }, { name: 'b', use: 0.5, req: 5, drop: ['container.memory.working_set'] }] }
  ]));
  const w = r.workloads[0];
  assert.strictEqual(w.status, 'partial');
  assert.ok(Math.abs(w.reqCores - 2) < 1e-9);
  assert.deepStrictEqual(w.containers.map(c => c.excluded), [false, true]);
});

test('node prices set the rates and the basis names the caveats', () => {
  const node = (type, cap, region) => ({
    resource: { attributes: [S('k8s.node.name', type), S('host.type', type), S('cloud.provider', 'aws'), S('thoras.node.capacity_type', cap), S('cloud.region', region), I('thoras.step_seconds', 300)] },
    scopeMetrics: [{ metrics: [
      series('k8s.node.cpu.allocatable', '{cpu}', [4, 4], 300),
      series('k8s.node.memory.allocatable', 'By', [16 * GIB, 16 * GIB], 300, { int: true })
    ] }]
  });
  const rates = model.pricing(model.parse(doc({ cpu: [1, 1], mem: [1, 1], nodes: [node('m5.xlarge', 'spot', 'eu-west-1')] })).nodes);
  // m5.xlarge spot: 0.192 at a 68% discount, over 4 vCPU and 16 GiB.
  assert.ok(Math.abs(rates.perCore - 0.06144 / 4) < 1e-9);
  assert.ok(Math.abs(rates.perGiB - 0.06144 / 16) < 1e-9);
  assert.strictEqual(rates.basis, '1 of 1 nodes at AWS us-east-1 on-demand and spot list prices; nodes outside us-east-1 are priced as if in it');
});

// Two hours of one container: the first hour rolled up to 5-minute points, the
// second at 1 minute, as the poll adapter writes it.
function mixedDoc({ cpuOld, cpuNew, peakOld, cpuReqOld = 2, cpuReqNew = 2 }) {
  const t0 = 1000000;
  const times = [];
  for (let i = 0; i < 12; i++) times.push(t0 + i * 300);
  for (let i = 0; i < 60; i++) times.push(t0 + 3600 + i * 60);
  const pts = (f, int) => times.map((t, i) => {
    const v = f(i, i < 12);
    return int ? { timeUnixNano: String(t * 1e9), asInt: String(v) } : { timeUnixNano: String(t * 1e9), asDouble: v };
  });
  const spans = { key: 'thoras.step_spans', value: { arrayValue: { values: [
    { stringValue: `${t0}/${t0 + 3600}/300` }, { stringValue: `${t0 + 3600}/${t0 + 7200}/60` }] } } };
  const base = [S('k8s.cluster.name', 'c'), S('thoras.source', 'kubectl-snapshot'), I('thoras.step_seconds', 60), I('thoras.fidelity_tier', 3), spans,
    S('k8s.namespace.name', 'ns'), S('k8s.deployment.name', 'api')];
  const metrics = [
    { name: 'container.cpu.usage', unit: '{cpu}', gauge: { dataPoints: pts((i, old) => (old ? cpuOld : cpuNew)) } },
    { name: 'container.memory.working_set', unit: 'By', sum: { dataPoints: pts(() => GIB, true) } },
    { name: 'k8s.container.cpu.request.desired', unit: '{cpu}', sum: { dataPoints: pts((i, old) => (old ? cpuReqOld : cpuReqNew)) } },
    { name: 'k8s.container.memory.request.desired', unit: 'By', sum: { dataPoints: pts(() => 4 * GIB, true) } }
  ];
  if (peakOld !== undefined) {
    metrics.push({ name: 'thoras.container.cpu.usage.max', unit: '{cpu}', gauge: { dataPoints: pts((i, old) => (old ? peakOld : cpuNew)) } });
  }
  return {
    resourceMetrics: [
      { resource: { attributes: base }, scopeMetrics: [{ metrics: [{ name: 'k8s.deployment.pod.desired', unit: '{pod}', sum: { dataPoints: pts(() => 1, true) } }] }] },
      { resource: { attributes: base.concat([S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics }] }
    ]
  };
}

test('step spans: rolled-up points cover their whole step and means are weighted by time', () => {
  // Request 4 cores for the first hour and 2 for the second: an even split in time.
  const r = model.analyze(mixedDoc({ cpuOld: 0.5, cpuNew: 0.5, cpuReqOld: 4, cpuReqNew: 2 }));
  const w = r.workloads[0];
  assert.strictEqual(w.status, 'ok');
  assert.ok(Array.from(w.series.reqCpu).every(Number.isFinite), 'no gaps in the rolled-up hour');
  assert.ok(Math.abs(w.reqCores - 3) < 1e-9, String(w.reqCores));
  assert.strictEqual(r.fidelity.stepSeconds, 300);
  assert.ok(r.series.reqCpu.every(Number.isFinite), 'cluster series has no gaps');
});

test('step spans: peaks come from the max series when present', () => {
  // The first hour averages 0.2 but peaked at 1.5; the target follows the peak.
  const withPeak = model.analyze(mixedDoc({ cpuOld: 0.2, cpuNew: 0.2, peakOld: 1.5 })).workloads[0];
  const without = model.analyze(mixedDoc({ cpuOld: 0.2, cpuNew: 0.2 })).workloads[0];
  assert.ok(Math.abs(withPeak.series.tgtCpu[5] - 1.65) < 1e-9, String(withPeak.series.tgtCpu[5]));
  assert.ok(Math.abs(without.series.tgtCpu[5] - 0.22) < 1e-9, String(without.series.tgtCpu[5]));
  assert.ok(withPeak.wasteCores < without.wasteCores);
});

test('short spans are stated in minutes', () => {
  assert.strictEqual(model.fidelityLabel(15, 120, false), '2 minutes at 15-second resolution');
  assert.strictEqual(model.fidelityLabel(60, 60, false), '1 minute at 1-minute resolution');
  assert.strictEqual(model.fidelityLabel(15, 45, false), '45 seconds at 15-second resolution');
  assert.strictEqual(model.fidelityLabel(60, 3540, false), '59 minutes at 1-minute resolution');
  assert.strictEqual(model.fidelityLabel(60, 3600, false), '1 hour at 1-minute resolution');
  assert.strictEqual(model.fidelityLabel(60, 5400, false), '1.5 hours at 1-minute resolution');
});

test('a polled file names its source as live polling', () => {
  const d = doc({ cpu: [1, 1, 1], mem: [1, 1, 1], step: 60 });
  d.resourceMetrics.forEach(rm => rm.resource.attributes.forEach(a => {
    if (a.key === 'thoras.source') a.value.stringValue = 'kubectl-snapshot';
  }));
  assert.strictEqual(model.analyze(d).fidelity.source, 'live polling');
  assert.strictEqual(model.analyze(doc({ cpu: [1, 1, 1], mem: [1, 1, 1] })).fidelity.source, 'test');
});

test('fidelity notes are explained per workload and summed up once', () => {
  const noted = (notes) => {
    const d = doc({ cpu: [1, 1, 1], mem: [1, 1, 1] });
    d.resourceMetrics[1].resource.attributes.push({ key: 'thoras.fidelity_notes', value: { arrayValue: { values: notes.map(n => ({ stringValue: n })) } } });
    return model.analyze(d);
  };
  const r = noted(['usage_split_from_pod_by_requests', 'a_note_from_a_later_version']);
  assert.deepStrictEqual(r.workloads[0].notes, ['usage_split_from_pod_by_requests']);
  assert.deepStrictEqual(r.fidelity.notes, [
    'Container usage in 1 workload is the pod total split between containers by their requests, because the source has no per-container usage.'
  ]);
  assert.strictEqual(model.notes[r.workloads[0].notes[0]].workload, 'Usage split from pod totals by requests');
  const plain = model.analyze(doc({ cpu: [1, 1, 1], mem: [1, 1, 1] }));
  assert.deepStrictEqual(plain.workloads[0].notes, []);
  assert.deepStrictEqual(plain.fidelity.notes, []);
});

test('the chart series carry per-step peak usage next to the mean', () => {
  const cpu = Array.from({ length: 60 }, (_, i) => (i % 15 === 0 ? 1 : 0.2));
  const r = model.analyze(doc({ cpu, mem: cpu.map(() => 1), step: 60, replicas: 2 }));
  const s = r.workloads[0].series;
  assert.strictEqual(s.peakCpu[0], 2);
  assert.strictEqual(s.peakCpu[1], s.useCpu[1]);
  assert.strictEqual(r.series.peakCpu.length, r.series.useCpu.length);
  // Folding into chart buckets keeps the highest peak and averages the rest.
  const chart = require('../src/js/chart.js');
  const d = chart.downsample({ t: [0, 60], reqCpu: [4, 4], tgtCpu: [2.2, 2.2], useCpu: [2, 0.4], peakCpu: [2, 0.4] }, chart.modes.cpu);
  assert.deepStrictEqual(d.p, [2, 0.4]);
  assert.deepStrictEqual(chart.legend('cpu').map((l) => l.name), ['Requested', 'Usage + 10%', 'Peak usage', 'Mean usage', 'Gap']);
  assert.deepStrictEqual(chart.legend('spend').map((l) => l.name), ['Current spend', 'Without waste', 'Waste']);
});

test('the cluster peak adds each workload highest point in a bucket', () => {
  // A 1-minute workload peaks once in each 5-minute bucket of a 5-minute workload.
  const fine = doc({ cpu: Array.from({ length: 10 }, (_, i) => (i % 5 === 2 ? 3 : 1)), mem: Array(10).fill(1), step: 60 });
  const coarse = doc({ cpu: [1, 1], mem: [1, 1], step: 300 });
  coarse.resourceMetrics.forEach(rm => rm.resource.attributes.forEach(a => {
    if (a.key === 'k8s.deployment.name') a.value.stringValue = 'worker';
  }));
  const r = model.analyze({ resourceMetrics: fine.resourceMetrics.concat(coarse.resourceMetrics) });
  assert.strictEqual(r.totals.counted, 2);
  assert.strictEqual(r.series.peakCpu[0], 3 + 1);
  assert.ok(r.series.useCpu[0] < r.series.peakCpu[0]);
});

function fixture() {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json'), 'utf8'));
}

const TOTALS = ['spend', 'savings', 'cores', 'gib', 'reqCores', 'reqGiB', 'workloads', 'counted'];

test('compute with the default levers is analyze, however often the levers changed', () => {
  const doc = fixture();
  const want = model.analyze(doc).totals;
  const data = model.prepare(doc);
  model.compute(data, { headroom: 1.4, peakWindow: 3600, namespaces: ['shop'], range: [0, 1], rates: { perCore: 1, perGiB: 1 } });
  const got = model.compute(data, model.defaultLevers).totals;
  for (const k of TOTALS) assert.strictEqual(got[k], want[k], k);
  assert.deepStrictEqual(model.compute(data).totals, got);
});

test('headroom and the peak window move the target', () => {
  const data = model.prepare(fixture());
  const base = model.compute(data).totals;
  const more = model.compute(data, { headroom: 1.5 }).totals;
  const longer = model.compute(data, { peakWindow: 4 * 3600 }).totals;
  assert.ok(more.savings < base.savings);
  assert.ok(longer.savings < base.savings);
  assert.strictEqual(more.spend, base.spend);
  assert.strictEqual(more.reqCores, base.reqCores);
});

test('set rates price the requests instead of the nodes', () => {
  const data = model.prepare(fixture());
  const base = model.compute(data);
  const r = model.compute(data, { rates: { perCore: base.pricing.perCore * 2, perGiB: base.pricing.perGiB * 2 } });
  assert.ok(Math.abs(r.totals.spend - 2 * base.totals.spend) < 1e-6);
  assert.strictEqual(r.pricing.custom, true);
  assert.match(r.pricing.basis, /per core-hour and .* per GiB-hour/);
  assert.strictEqual(model.compute(data).pricing.basis, base.pricing.basis);
});

test('a namespace scope counts only its workloads', () => {
  const data = model.prepare(fixture());
  const all = model.compute(data);
  const shop = model.compute(data, { namespaces: ['shop'] });
  assert.deepStrictEqual(shop.workloads.map(w => w.ns), ['shop']);
  assert.strictEqual(shop.totals.workloads, 1);
  const w = all.workloads.find(x => x.ns === 'shop');
  assert.strictEqual(shop.totals.savings, w.annualSavings);
  assert.deepStrictEqual(shop.namespaces, all.namespaces);
});

test('a time range averages only its points and clips the cluster series', () => {
  const data = model.prepare(fixture());
  const all = model.compute(data);
  const t = all.series.t;
  const from = t[100], to = t[400];
  const r = model.compute(data, { range: [from, to] });
  assert.strictEqual(r.series.t[0], from);
  assert.strictEqual(r.series.t[r.series.t.length - 1], to);
  assert.notStrictEqual(r.totals.savings, all.totals.savings);
  const outside = model.compute(data, { range: [1, 2] });
  assert.strictEqual(outside.totals.counted, 0);
  assert.ok(outside.workloads.every(w => w.status === 'excluded'));
  assert.ok(outside.workloads.some(w => w.reason === 'No data in the selected time range'));
});

test('a range covering the whole file gives the same totals as no range', () => {
  const data = model.prepare(fixture());
  const all = model.compute(data);
  const t = all.series.t;
  for (const range of [[t[0], t[t.length - 1]], [data.fidelity.start, data.fidelity.end], [0, 4e9]]) {
    const r = model.compute(data, { range });
    for (const k of TOTALS) assert.strictEqual(r.totals[k], all.totals[k], k + ' ' + range);
  }
});

test('a range clips each workload series as well as the cluster series', () => {
  const data = model.prepare(fixture());
  const t = model.compute(data).series.t;
  const r = model.compute(data, { range: [t[10], t[20]] });
  for (const w of r.workloads.filter(x => x.series)) {
    assert.strictEqual(w.series.t[0], t[10]);
    assert.strictEqual(w.series.t[w.series.t.length - 1], t[20]);
  }
});

test('nodes are priced from their own cloud table, spot and custom types the way the product prices them', () => {
  const price = (hostType, provider, capacity, extra) => model.priceNode(Object.assign({ hostType, provider, capacity, region: '', cores: 3.9, gib: 14 }, extra));
  const near = (a, b) => Math.abs(a - b) <= 1e-12;
  let p = price('n2-standard-4', 'gcp', 'on_demand');
  assert.deepStrictEqual([p.cloud, p.cores, p.gib, p.hourly, p.reason], ['gcp', 4, 16, 0.218756, '']);
  p = price('e2-standard-4', 'gcp', 'spot');
  assert.ok(p.spot && near(p.hourly, 0.07547082));
  p = price('n2d-custom-16-32768', 'gcp', 'on_demand');
  assert.ok(p.custom && p.cores === 16 && p.gib === 32 && near(p.hourly, 16 * 0.0325248 + 32 * 0.00435855));
  p = price('n2d-custom-4-8192', 'gcp', 'spot');
  assert.ok(near(p.hourly, 4 * 0.01015 + 8 * 0.001663));
  p = price('Standard_D4s_v5', 'azure', 'spot');
  assert.ok(!p.spot && p.azureSpot && p.hourly === 0.192);
  p = price('m5.2xlarge', 'aws', 'spot', { region: 'us-west-2' });
  assert.ok(p.spot && p.otherRegion && near(p.hourly, 0.13824));
  assert.strictEqual(price('c8ib.large', 'aws', 'spot').reason, 'spot');
  assert.strictEqual(price('x9.unknown', 'aws', 'on_demand').reason, 'type');
  assert.strictEqual(price('VM.Standard.E4.Flex', 'oci', 'on_demand').reason, 'cloud');
  p = price('c5.large', 'unknown', 'unknown');
  assert.ok(p.inferred && p.cloud === 'aws' && p.hourly === 0.085);
  const rates = model.pricing([
    { hostType: 'n2-standard-4', provider: 'gcp', capacity: 'on_demand' },
    { hostType: 'Standard_D4s_v5', provider: 'azure', capacity: 'on_demand', region: 'westeurope' }
  ]);
  assert.ok(near(rates.perCore, (0.218756 + 0.192) / 8) && near(rates.perGiB, (0.218756 + 0.192) / 32));
  assert.strictEqual(rates.basis, '2 of 2 nodes at Google Cloud us-central1 and Azure eastus on-demand list prices; nodes outside those regions are priced as if in them');
});
