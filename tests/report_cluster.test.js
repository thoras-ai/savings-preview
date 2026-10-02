'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');
const state = require('../src/js/state.js');
const cluster = require('../src/js/section-cluster.js');
const { budget } = require('./budget');

const GIB = 1 << 30;
const T0 = 1789516800;
const S = (k, v) => ({ key: k, value: { stringValue: v } });
const I = (k, v) => ({ key: k, value: { intValue: String(v) } });
const close = (a, b, msg) => assert.ok(Math.abs(a - b) < 1e-9, (msg || '') + ` ${a} != ${b}`);

function fixture() {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json'), 'utf8'));
}

function figures(data, hash) {
  const st = state.parse(hash || '');
  const r = model.compute(data, state.levers(st));
  return { r, s: cluster.summarize(data, r, st) };
}

function points(n, step, f, int) {
  return Array.from({ length: n }, (_, i) => {
    const p = { timeUnixNano: String((T0 + i * step) * 1e9) };
    if (int) p.asInt = String(Math.round(f(i))); else p.asDouble = f(i);
    return p;
  });
}

// One-container deployments and nodes, all at one step. A node's from/to
// are point indexes, for nodes that join or leave part way.
function doc({ n = 12, step = 300, workloads, nodes = [] }) {
  const rms = [];
  workloads.forEach((w, k) => {
    const base = [S('k8s.cluster.name', 'c'), I('thoras.step_seconds', step), I('thoras.fidelity_tier', w.tier || 2),
      S('k8s.namespace.name', w.ns || 'ns'), S('k8s.deployment.name', w.name || 'w' + k)];
    rms.push({ resource: { attributes: base }, scopeMetrics: [{ metrics: [{ name: 'k8s.deployment.pod.desired', sum: { dataPoints: points(n, step, () => w.replicas || 1, true) } }] }] });
    rms.push({ resource: { attributes: base.concat([S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics: [
      { name: 'container.cpu.usage', gauge: { dataPoints: points(n, step, () => w.cpu) } },
      { name: 'container.memory.working_set', sum: { dataPoints: points(n, step, () => w.mem * GIB, true) } },
      { name: 'k8s.container.cpu.request.desired', sum: { dataPoints: points(n, step, () => w.cpuReq) } },
      { name: 'k8s.container.memory.request.desired', sum: { dataPoints: points(n, step, () => w.memReq * GIB, true) } }
    ] }] });
  });
  nodes.forEach((nd, k) => {
    const from = nd.from || 0, to = nd.to === undefined ? n : nd.to;
    const pts = (f, int) => points(n, step, f, int).slice(from, to);
    const attrs = [S('k8s.node.name', 'node-' + k), S('host.type', nd.type), S('thoras.node.capacity_type', nd.capacity || 'on_demand'), I('thoras.step_seconds', step)];
    if (nd.region) attrs.push(S('cloud.region', nd.region));
    rms.push({ resource: { attributes: attrs }, scopeMetrics: [{ metrics: [
      { name: 'k8s.node.cpu.allocatable', sum: { dataPoints: pts(() => nd.cores) } },
      { name: 'k8s.node.memory.allocatable', sum: { dataPoints: pts(() => nd.gib * GIB, true) } }
    ] }] });
  });
  return { resourceMetrics: rms };
}

test('at the default levers the section shows the totals the Go summary reports', () => {
  const data = model.prepare(fixture());
  const { r, s } = figures(data);
  close(s.cpu.req, r.totals.reqCores);
  close(s.mem.req, r.totals.reqGiB);
  close(s.cpu.req - s.cpu.target, r.totals.cores, 'cpu waste');
  close(s.mem.req - s.mem.target, r.totals.gib, 'memory waste');
  assert.strictEqual(s.nodes, 2);
  close(s.alloc.cores, 3.92 + 7.91, 'allocatable cores');
  assert.ok(Math.abs(s.alloc.gib - (14.8 + 30.1)) < 1e-6, String(s.alloc.gib));
  close(s.freed.cpu, r.totals.cores / ((3.92 + 7.91) / 2));
  assert.strictEqual(s.freed.nodes, Math.min(s.freed.cpu, s.freed.mem));
  assert.strictEqual(s.freed.whole, 0);
});

test('mean usage is time weighted and summed over workloads', () => {
  const data = model.prepare(doc({ workloads: [
    { cpu: 0.5, mem: 1, cpuReq: 2, memReq: 4, replicas: 3 },
    { cpu: 0.25, mem: 2, cpuReq: 1, memReq: 4 }
  ] }));
  const { r, s } = figures(data);
  close(r.workloads[0].useCores, 1.5);
  close(r.totals.useCores, 1.75);
  close(r.totals.useGiB, 5);
  close(s.cpu.used, 1.75);
});

test('the node mix lists list prices from the shared price table', () => {
  const data = model.prepare(doc({
    workloads: [{ cpu: 1, mem: 1, cpuReq: 2, memReq: 2 }],
    nodes: [{ type: 'm5.xlarge', cores: 3.92, gib: 14.8 }, { type: 'm5.xlarge', cores: 3.92, gib: 14.8 },
      { type: 'm5.2xlarge', cores: 7.91, gib: 30.1, capacity: 'spot' }, { type: 'x9.custom', cores: 2, gib: 8 }]
  }));
  const { s } = figures(data);
  assert.deepStrictEqual(s.mix.map(g => [g.type, g.count, g.capacity, g.hourly]), [
    ['m5.xlarge', 2, 'on-demand', model.nodePrices['m5.xlarge']],
    ['m5.2xlarge', 1, 'spot', model.nodePrices['m5.2xlarge']],
    ['x9.custom', 1, 'on-demand', undefined]
  ]);
  assert.strictEqual(s.priced, 3);
  const html = cluster.markup(s, false);
  assert.match(html, /\$0\.192/);
  assert.match(html, /\$0\.384/);
  assert.match(html, /Not priced/);
  assert.match(html, /Spot nodes are priced at the on-demand list price/);
  assert.match(html, /left out of the per-core and per-GiB rates/);
});

test('allocatable is averaged over the window, so a node there for half of it counts half', () => {
  const data = model.prepare(doc({
    workloads: [{ cpu: 1, mem: 1, cpuReq: 2, memReq: 2 }],
    nodes: [{ type: 'm5.xlarge', cores: 4, gib: 16 }, { type: 'm5.xlarge', cores: 4, gib: 16, from: 6 }]
  }));
  close(figures(data).s.alloc.cores, 6);
  // A time range covering only the second half sees both nodes.
  const half = figures(data, '#from=' + (T0 + 6 * 300) + '&to=' + (T0 + 12 * 300)).s;
  close(half.alloc.cores, 8);
  close(half.alloc.gib, 32);
});

test('a held node series holds each value until the next point', () => {
  const nd = { t: Float64Array.from([0, 100]), v: Float64Array.from([2, 4]) };
  const held = { series: { x: nd }, held: { x: true }, spans: null, step: 10 };
  close(cluster.levelOver(held, 'x', 0, 200), (2 * 100 + 4 * 10) / 200);
  const dense = { series: { x: nd }, held: {}, spans: null, step: 10 };
  close(cluster.levelOver(dense, 'x', 0, 200), (2 * 10 + 4 * 10) / 200);
});

test('the levers move the figures', () => {
  const data = model.prepare(fixture());
  const base = figures(data).s;
  const more = figures(data, '#headroom=50').s;
  assert.strictEqual(more.cpu.req, base.cpu.req);
  assert.ok(more.cpu.target > base.cpu.target);
  assert.ok(more.freed.nodes < base.freed.nodes);
  const shop = figures(data, '#ns=shop').s;
  assert.ok(shop.cpu.req < base.cpu.req);
  assert.ok(shop.filtered);
  assert.match(cluster.markup(shop, false), /selected namespaces/);
  assert.deepStrictEqual(shop.alloc, base.alloc);
  const priced = figures(data, '#cpu=0.05&gib=0.006').s;
  assert.ok(priced.customRates);
  assert.match(cluster.markup(priced, false), /rates set under Assumptions/);
  assert.strictEqual(priced.priced, base.priced);
});

test('requests above allocatable and uncounted workloads are explained', () => {
  const html = cluster.markup(figures(model.prepare(fixture())).s, false);
  assert.match(html, /request more CPU than the nodes in the file can allocate/);
  assert.match(html, /1 workload with too little data is not in the requested and used figures/);
  // The bar stops at allocatable.
  assert.match(html, /class="cap-req" style="width:100\.0%"/);
});

test('without nodes the bar is scaled to requests and freed nodes use the default node', () => {
  const data = model.prepare(doc({ workloads: [{ cpu: 0.5, mem: 1, cpuReq: 4, memReq: 8 }] }));
  const { r, s } = figures(data);
  assert.strictEqual(s.alloc, null);
  assert.strictEqual(s.freed.avg.fromNodes, false);
  close(s.freed.cpu, r.totals.cores / 4);
  close(s.freed.mem, r.totals.gib / 16);
  const html = cluster.markup(s, false);
  assert.match(html, /The file lists no nodes/);
  assert.match(html, /the bar is what the workloads request/);
  assert.match(html, /default node \(4 cores, 16 GiB\)/);
  assert.match(html, /class="cap-req" style="width:100\.0%"/);
});

test('whole nodes are counted when the waste covers them', () => {
  const data = model.prepare(doc({
    workloads: [{ cpu: 0.1, mem: 0.1, cpuReq: 10, memReq: 40 }],
    nodes: [1, 2, 3].map(() => ({ type: 'm5.xlarge', cores: 4, gib: 16 }))
  }));
  const { s } = figures(data);
  assert.strictEqual(s.freed.whole, 2);
  assert.match(cluster.markup(s, false), /2 whole nodes/);
});

test('a time range with no data leaves the figures empty rather than zero', () => {
  const data = model.prepare(fixture());
  const s = figures(data, '#from=1&to=2').s;
  assert.ok(Number.isNaN(s.cpu.req));
  assert.strictEqual(s.alloc, null);
  assert.match(cluster.markup(s, false), /nothing to compare with the nodes/);
});

test('provisional figures are rounded and marked', () => {
  const data = model.prepare(doc({
    n: 1, workloads: [{ cpu: 0.123, mem: 1.234, cpuReq: 1.234, memReq: 4.567, tier: 3 }],
    nodes: [{ type: 'm5.xlarge', cores: 3.92, gib: 14.8 }]
  }));
  const html = cluster.markup(figures(data).s, true);
  assert.match(html, /~1\.2 cores · ~4\.6 GiB/);
  assert.match(html, /kpi-provisional/);
});

test('at 1,000 workloads a lever change re-renders the section well inside the budget', () => {
  const step = 900, n = 14 * 86400 / step;
  const workloads = Array.from({ length: 1000 }, (_, k) => ({ ns: 'ns-' + (k % 20), cpu: 0.1 + (k % 7) / 20, mem: 0.5 + (k % 5) / 10, cpuReq: 0.5, memReq: 2, replicas: 2 }));
  const nodes = Array.from({ length: 60 }, (_, k) => ({ type: k % 2 ? 'm5.2xlarge' : 'r5.xlarge', cores: 7.9, gib: 30 }));
  const data = model.prepare(doc({ n, step, workloads, nodes }));
  for (const hash of ['', '#headroom=30', '#ns=ns-1,ns-2', '#peak=60']) {
    const st = state.parse(hash), r = model.compute(data, state.levers(st));
    const t0 = performance.now();
    const html = cluster.markup(cluster.summarize(data, r, st), false);
    const ms = performance.now() - t0;
    assert.ok(ms < budget(50), `${hash || 'defaults'}: ${ms.toFixed(1)} ms`);
    assert.match(html, /<div class="kpi-value">60</);
  }
});

test('node equivalents round down, so they never read above the whole nodes', () => {
  const s = { freed: { nodes: 38.99, cpu: 62.31, mem: 38.99, whole: 38, avg: { cores: 6.6, gib: 30.3, fromNodes: true } },
    nodes: 120, types: 1, priced: 120, alloc: null, cpu: { req: NaN }, mem: { req: NaN }, mix: [], left: 0 };
  const html = cluster.markup(s, false);
  assert.match(html, />38\.9 nodes</);
  assert.match(html, /38 whole nodes/);
  assert.match(html, /free 62\.3 nodes and memory waste 38\.9;/);
});

test('an amount too small for two decimals is not shown as zero', () => {
  const data = model.prepare(doc({ workloads: [{ cpu: 0.001, mem: 0.003, cpuReq: 0.5, memReq: 1 }], nodes: [{ type: 'm5.xlarge', cores: 4, gib: 16 }] }));
  const html = cluster.markup(figures(data).s, false);
  assert.match(html, /&lt;0\.01 cores · &lt;0\.01 GiB/);
});

test('a workload using more than it requests is explained, since right-sized requests never rise', () => {
  const data = model.prepare(doc({ workloads: [{ cpu: 0.3, mem: 0.1, cpuReq: 0.2, memReq: 1 }], nodes: [{ type: 'm5.xlarge', cores: 4, gib: 16 }] }));
  const { s } = figures(data);
  assert.ok(s.cpu.used > s.cpu.target);
  close(s.cpu.target, 0.2);
  const html = cluster.markup(s, false);
  assert.match(html, /Right-sized requests/);
  assert.match(html, /use more CPU than right-sized requests in total/);
  assert.doesNotMatch(html, /use more CPU and memory/);
});

test('one core reads in the singular, rounded or not', () => {
  const data = model.prepare(doc({ n: 1, workloads: [{ cpu: 0.2, mem: 1, cpuReq: 1.04, memReq: 2, tier: 3 }] }));
  assert.match(cluster.markup(figures(data).s, true), /~1 core · ~2 GiB/);
  assert.match(cluster.markup(figures(data).s, false), />1 core · 2 GiB</);
});

test('nodes without an instance type are not counted as a type', () => {
  const data = model.prepare(doc({ workloads: [{ cpu: 1, mem: 1, cpuReq: 2, memReq: 2 }], nodes: [{ type: 'unknown', cores: 4, gib: 16 }] }));
  const s = figures(data).s;
  assert.strictEqual(s.types, 0);
  assert.match(cluster.markup(s, false), /Instance type not stated/);
});

test('workloads outside the time range are not described as lacking data', () => {
  const data = model.prepare(fixture());
  const t = model.compute(data).series.t;
  const s = figures(data, '#from=' + t[10] + '&to=' + t[20]).s;
  assert.strictEqual(s.left, 1);
  assert.strictEqual(figures(data, '#ns=shop').s.left, 0);
});

test('without nodes the whole-node copy names the default node size', () => {
  const data = model.prepare(doc({ workloads: [{ cpu: 0.1, mem: 0.1, cpuReq: 10, memReq: 40 }] }));
  const html = cluster.markup(figures(data).s, false);
  assert.match(html, /removed at the default node size/);
  assert.doesNotMatch(html, /average/);
});
