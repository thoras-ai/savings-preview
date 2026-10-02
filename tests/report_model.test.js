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
