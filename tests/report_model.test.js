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
});

function multi(workloads, nodes = []) {
  const rms = [];
  workloads.forEach(({ name, replicas, containers, cpu }) => {
    const base = [S('k8s.cluster.name', 'c'), I('thoras.step_seconds', 300), I('thoras.fidelity_tier', 2), S('k8s.namespace.name', 'ns'), S('k8s.deployment.name', name)];
    const n = cpu.length;
    const first = containers[0].first || 0;
    rms.push({ resource: { attributes: base }, scopeMetrics: [{ metrics: [series('k8s.deployment.pod.desired', '{pod}', Array(n).fill(replicas), 300, { int: true })] }] });
    containers.forEach(c => {
      const pad = Array(first).fill(null);
      const v = (x) => pad.concat(Array(n - first).fill(x));
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
    resource: { attributes: [S('k8s.node.name', type), S('host.type', type), S('thoras.node.capacity_type', cap), S('cloud.region', region), I('thoras.step_seconds', 300)] },
    scopeMetrics: [{ metrics: [
      series('k8s.node.cpu.allocatable', '{cpu}', [4, 4], 300),
      series('k8s.node.memory.allocatable', 'By', [16 * GIB, 16 * GIB], 300, { int: true })
    ] }]
  });
  const rates = model.pricing(model.parse(doc({ cpu: [1, 1], mem: [1, 1], nodes: [node('m5.xlarge', 'spot', 'eu-west-1')] })).nodes);
  assert.ok(Math.abs(rates.perCore - 0.192 / 4) < 1e-9);
  assert.match(rates.basis, /us-east-1/);
  assert.match(rates.basis, /spot/);
  assert.match(rates.basis, /outside us-east-1/);
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
