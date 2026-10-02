'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');
const state = require('../src/js/state.js');
const wl = require('../src/js/section-workloads.js');

const GIB = 1 << 30;
const T0 = 1000000;
const S = (k, v) => ({ key: k, value: { stringValue: v } });
const I = (k, v) => ({ key: k, value: { intValue: String(v) } });

function series(name, values, step, opts = {}) {
  const pts = [];
  values.forEach((v, i) => {
    if (v === null) return;
    const p = { timeUnixNano: String((T0 + i * step) * 1e9) };
    if (opts.int) p.asInt = String(v); else p.asDouble = v;
    pts.push(p);
  });
  return { name, sum: { dataPoints: pts } };
}

// One deployment with one container. cpu and mem are per-pod usage in cores
// and GiB; limits are per pod; replicas is a count or one per step; oom maps a
// step index to kills in that step.
function doc({ cpu, mem, cpuReq = 1, memReq = 4, replicas = 2, step = 300, tier = 1, cpuLim, memLim, oom = {}, name = 'api', ns = 'ns' }) {
  const base = [S('k8s.cluster.name', 'c'), S('thoras.source', 'test'), I('thoras.step_seconds', step), I('thoras.fidelity_tier', tier)];
  const id = [S('k8s.namespace.name', ns), S('k8s.deployment.name', name)];
  const metrics = [
    series('container.cpu.usage', cpu, step),
    series('container.memory.working_set', mem.map(v => v * GIB), step, { int: true }),
    series('k8s.container.cpu.request.desired', cpu.map(() => cpuReq), step),
    series('k8s.container.memory.request.desired', cpu.map(() => memReq * GIB), step, { int: true })
  ];
  if (cpuLim !== undefined) metrics.push(series('k8s.container.cpu.limit.desired', cpu.map(() => cpuLim), step));
  if (memLim !== undefined) metrics.push(series('k8s.container.memory.limit.desired', cpu.map(() => memLim * GIB), step, { int: true }));
  if (Object.keys(oom).length) metrics.push(series('thoras.container.oom_kills', cpu.map((_, i) => oom[i] || null), step, { int: true }));
  return {
    resourceMetrics: [
      { resource: { attributes: base.concat(id) }, scopeMetrics: [{ metrics: [series('k8s.deployment.pod.desired', cpu.map((_, i) => (Array.isArray(replicas) ? replicas[i] : replicas)), step, { int: true })] }] },
      { resource: { attributes: base.concat(id, [S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics }] }
    ]
  };
}

function fixture() {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json'), 'utf8'));
}

function ctxFor(result, st = {}) {
  return { result, state: Object.assign({}, state.DEFAULTS, st), approx: false };
}

const close = (a, b, msg) => assert.ok(Math.abs(a - b) < 1e-9, `${msg}: ${a} != ${b}`);

test('the workload rows add up to the totals at the defaults', () => {
  const data = model.prepare(fixture());
  const r = model.compute(data);
  const rows = wl.rows(data, r);
  const sum = k => rows.filter(x => x.status !== 'excluded').reduce((s, x) => s + x[k], 0);
  assert.strictEqual(sum('annualSpend'), r.totals.spend);
  assert.strictEqual(sum('annualSavings'), r.totals.savings);
  assert.strictEqual(sum('wasteCores'), r.totals.cores);
  assert.strictEqual(sum('wasteGiB'), r.totals.gib);
  assert.strictEqual(rows.length, r.totals.workloads);
});

test('the fixture rows carry replicas, OOM kills and status', () => {
  const data = model.prepare(fixture());
  const rows = wl.rows(data, model.compute(data));
  const by = Object.fromEntries(rows.map(x => [x.name, x]));
  assert.strictEqual(by['checkout-api'].replicas, 6);
  assert.strictEqual(by['search-indexer'].oomKills, 2);
  assert.strictEqual(by['checkout-api'].oomKills, 0);
  assert.strictEqual(by['report-worker'].status, 'excluded');
  assert.ok(Number.isNaN(by['report-worker'].replicas));
});

test('drill-down figures are per pod, with the target, peak-safe value and limits', () => {
  // Per-pod CPU usage steps between 0.2 and 0.6, at 1-minute steps and 2 replicas.
  const cpu = Array.from({ length: 60 }, (_, i) => (i < 30 ? 0.2 : 0.6));
  const mem = cpu.map(() => 1);
  const data = model.prepare(doc({ cpu, mem, step: 60, cpuLim: 2, memLim: 3, oom: { 10: 1, 40: 2 } }));
  const r = model.compute(data);
  const d = wl.detail(data, r, 'ns/deployment/api');
  assert.strictEqual(d.counted, true);
  assert.strictEqual(d.replicas, 2);
  close(d.cpu.now, 1, 'cpu now');
  close(d.mem.now, 4, 'mem now');
  close(d.cpu.requested, 2, 'cpu requested is the total across replicas');
  close(d.cpu.mean, 0.8, 'mean usage across replicas');
  close(d.cpu.peak, 1.2, 'peak usage across replicas');
  close(d.cpu.peakSafe, 0.66, 'peak-safe is the highest per-pod peak plus 10%');
  // The 15-minute peak trails a rise by up to 15 steps: 0.2 for 30 steps, 0.6 for 30.
  close(d.cpu.target, 1.1 * (0.2 * 30 + 0.6 * 30) / 60, 'target');
  close(d.mem.target, 1.1, 'memory target');
  assert.strictEqual(d.cpu.limit, 2);
  assert.strictEqual(d.mem.limit, 3);
  assert.strictEqual(d.oom.count, 3);
  assert.deepStrictEqual(d.oom.marks.map(m => m.t), [T0 + 600, T0 + 2400]);
  assert.match(d.oom.marks[1].label, /^2 OOM kills, /);
});

test('limits are unknown when a container sets none', () => {
  const cpu = [0.5, 0.5, 0.5];
  const data = model.prepare(doc({ cpu, mem: [1, 1, 1] }));
  const d = wl.detail(data, model.compute(data), 'ns/deployment/api');
  assert.strictEqual(d.cpu.limit, null);
  assert.strictEqual(d.mem.limit, null);
  assert.strictEqual(d.oom.count, 0);
});

test('headroom moves the target and peak-safe value; the time range moves replicas and OOM kills', () => {
  const cpu = Array.from({ length: 60 }, (_, i) => (i < 30 ? 0.2 : 0.6));
  const data = model.prepare(doc({ cpu, mem: cpu.map(() => 1), step: 60, oom: { 10: 1, 40: 2 } }));
  const id = 'ns/deployment/api';
  const base = wl.detail(data, model.compute(data), id);
  const more = wl.detail(data, model.compute(data, { headroom: 1.5 }), id);
  close(more.cpu.peakSafe, base.cpu.peakSafe / 1.1 * 1.5, 'peak-safe follows headroom');
  assert.ok(more.cpu.target > base.cpu.target);
  const late = model.compute(data, { range: [T0 + 1800, T0 + 3540] });
  const d = wl.detail(data, late, id);
  assert.strictEqual(d.oom.count, 2);
  close(d.cpu.mean, 1.2, 'mean over the range only');
  assert.strictEqual(wl.rows(data, late)[0].oomKills, 2);
});

test('risks name OOM kills and limits below the peak-safe value', () => {
  const cpu = Array.from({ length: 30 }, () => 0.5);
  const data = model.prepare(doc({ cpu, mem: cpu.map(() => 3), step: 60, cpuLim: 0.5, memLim: 3, oom: { 5: 2 } }));
  const r = model.compute(data);
  const risks = wl.risks(wl.detail(data, r, 'ns/deployment/api'), r.fidelity);
  assert.deepStrictEqual(risks.map(x => x.tone), ['danger', 'caution', 'caution']);
  assert.match(risks[0].text, /^2 OOM kills in the window\. Memory is already tight at the current limit/);
  assert.match(risks[1].text, /memory limit \(3 GiB per pod\) is below the peak-safe value \(3\.3 GiB\)/);
  assert.match(risks[2].text, /CPU limit \(0\.5 cores per pod\) is below the peak-safe value \(0\.55 cores\)/);
});

test('a workload with room to spare and full history has no risks', () => {
  const cpu = Array.from({ length: 30 }, () => 0.2);
  const data = model.prepare(doc({ cpu, mem: cpu.map(() => 1), step: 60, cpuLim: 2, memLim: 8 }));
  const r = model.compute(data);
  assert.deepStrictEqual(wl.risks(wl.detail(data, r, 'ns/deployment/api'), r.fidelity), []);
});

test('provisional data is a risk of its own', () => {
  const cpu = [0.2, 0.2];
  const data = model.prepare(doc({ cpu, mem: [1, 1], tier: 3 }));
  const r = model.compute(data);
  const risks = wl.risks(wl.detail(data, r, 'ns/deployment/api'), r.fidelity);
  assert.strictEqual(risks.length, 1);
  assert.strictEqual(risks[0].tone, 'caution');
});

test('the list filters by name or namespace, keeps uncounted workloads last and pages', () => {
  const data = model.prepare(fixture());
  const r = model.compute(data);
  const rows = wl.rows(data, r);
  const view = (o) => Object.assign({ sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0 }, o);
  assert.deepStrictEqual(wl.arrange(rows, view()).map(x => x.name), ['checkout-api', 'ledger-db', 'search-indexer', 'report-worker']);
  assert.deepStrictEqual(wl.arrange(rows, view({ sort: { key: 'annualSavings', dir: 1 } })).map(x => x.name), ['search-indexer', 'ledger-db', 'checkout-api', 'report-worker']);
  assert.deepStrictEqual(wl.arrange(rows, view({ filter: ' SEARCH ' })).map(x => x.name), ['search-indexer']);
  assert.deepStrictEqual(wl.arrange(rows, view({ filter: 'batch' })).map(x => x.name), ['report-worker']);
  const parts = wl.listParts(rows, view(), ctxFor(r));
  assert.strictEqual(parts.title, '3 of 4 workloads counted');
  assert.strictEqual(parts.range, '1–4 of 4');
  assert.strictEqual(parts.prev || parts.next, false);
  const none = wl.listParts(rows, view({ filter: 'nothing' }), ctxFor(r));
  assert.strictEqual(none.range, '0 of 4');
  assert.match(none.table, /No workload matches “nothing”/);
});

test('names are escaped and each row links to its drill-down with the settings kept', () => {
  const data = model.prepare(doc({ cpu: [0.5, 0.5], mem: [1, 1], name: '<b>x</b>' }));
  const r = model.compute(data, { headroom: 1.3 });
  const ctx = ctxFor(r, { headroom: 0.3 });
  const parts = wl.listParts(wl.rows(data, r), { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0 }, ctx);
  assert.ok(!parts.table.includes('<b>x</b>'));
  assert.match(parts.table, /href="#section=workloads&amp;headroom=30&amp;workload=ns\/deployment\/%3Cb%3Ex%3C\/b%3E"/);
  const html = wl.detailHtml(wl.detail(data, r, 'ns/deployment/<b>x</b>'), { mode: 'cpu' }, ctx);
  assert.ok(!html.includes('<b>x</b>'));
  assert.match(html, /Target: peak \+ 30%/);
});

test('an uncounted workload opens on what it is missing', () => {
  const data = model.prepare(fixture());
  const r = model.compute(data);
  const d = wl.detail(data, r, 'batch/deployment/report-worker');
  assert.strictEqual(d.counted, false);
  const html = wl.detailHtml(d, { mode: 'cpu' }, ctxFor(r));
  assert.match(html, /Not counted\./);
  assert.match(html, /container\.memory\.working_set/);
  assert.strictEqual(wl.detail(data, r, 'no/such/workload'), null);
});

// A 14-day compact sample with 1,000 workloads: one workload at the 15-minute
// compact step, prepared once and copied, so compute and the section do the
// work of a real file without parsing one.
function largeSample(count) {
  const n = 14 * 96;
  const cpu = Array.from({ length: n }, (_, i) => 0.3 + 0.2 * Math.sin(i / 96 * 2 * Math.PI) + (i % 37 === 0 ? 0.3 : 0));
  const one = model.prepare(doc({ cpu, mem: cpu.map(v => 2 + v), step: 900, cpuLim: 1, memLim: 4, oom: { 100: 1 } }));
  const p = one.workloads[0];
  const copy = (a) => a && a.slice();
  const workloads = [];
  for (let i = 0; i < count; i++) {
    const s = {};
    for (const k of Object.keys(p.s)) s[k] = ArrayBuffer.isView(p.s[k]) ? p.s[k].slice() : p.s[k];
    const ns = 'ns-' + (i % 25), name = 'app-' + i;
    workloads.push(Object.assign({}, p, {
      id: ns + '/deployment/' + name, ns, name, s, peaks: null,
      weight: copy(p.weight), wasteCpu: copy(p.wasteCpu), wasteMem: copy(p.wasteMem),
      replicas: copy(p.replicas), limCpu: copy(p.limCpu), limMem: copy(p.limMem)
    }));
  }
  return Object.assign({}, one, { workloads, namespaces: Array.from(new Set(workloads.map(w => w.ns))).sort() });
}

test('the section renders 1,000 workloads within the page budget', () => {
  const data = largeSample(1000);
  const view = { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0, mode: 'cpu' };
  model.compute(data);
  const r = model.compute(data, { headroom: 1.2 });
  const t0 = performance.now();
  const rows = wl.rows(data, r);
  const parts = wl.listParts(rows, view, ctxFor(r));
  const tList = performance.now() - t0;
  assert.strictEqual(parts.range, '1–50 of 1,000');
  assert.strictEqual(parts.next, true);
  const t1 = performance.now();
  wl.listParts(rows, Object.assign({}, view, { filter: 'app-99', sort: { key: 'name', dir: 1 } }), ctxFor(r));
  const tFilter = performance.now() - t1;
  const t2 = performance.now();
  const d = wl.detail(data, r, data.workloads[500].id);
  wl.detailHtml(d, view, ctxFor(r));
  require('../src/js/chart.js').downsample(d.w.series, require('../src/js/chart.js').modes.cpu);
  const tDetail = performance.now() - t2;
  // A lever change has about 200 ms for compute and every section together.
  assert.ok(tList < 100, `list ${tList.toFixed(1)} ms`);
  assert.ok(tFilter < 50, `filter ${tFilter.toFixed(1)} ms`);
  assert.ok(tDetail < 50, `drill-down ${tDetail.toFixed(1)} ms`);
});

test('the spend view leaves OOM kills out of its legend, and tiny usage is not shown as 0', () => {
  const cpu = [0.0021, 0.0021, 0.0021];
  const data = model.prepare(doc({ cpu, mem: [1, 1, 1], oom: { 1: 1 } }));
  const r = model.compute(data);
  const d = wl.detail(data, r, 'ns/deployment/api');
  assert.match(wl.detailHtml(d, { mode: 'cpu' }, ctxFor(r)), /OOM kill<\/span>/);
  assert.doesNotMatch(wl.detailHtml(d, { mode: 'spend' }, ctxFor(r)), /OOM kill<\/span>/);
  assert.match(wl.detailHtml(d, { mode: 'cpu' }, ctxFor(r)), /0\.0042 \/ 0\.0042 cores/);
});

test('time scaled to zero does not lower the per-pod request or target', () => {
  const cpu = Array.from({ length: 48 }, () => 0.5);
  const replicas = cpu.map((_, i) => (i < 24 ? 2 : 0));
  const data = model.prepare(doc({ cpu, mem: cpu.map(() => 1), step: 60, replicas }));
  const d = wl.detail(data, model.compute(data), 'ns/deployment/api');
  close(d.cpu.now, 1, 'cpu now');
  close(d.mem.now, 4, 'mem now');
  close(d.cpu.target, 0.55, 'cpu target');
  close(d.cpu.peakSafe, 0.55, 'cpu peak-safe');
});

test('a kill stamped at the start of a range belongs to the range before it', () => {
  const cpu = Array.from({ length: 30 }, () => 0.5);
  const data = model.prepare(doc({ cpu, mem: cpu.map(() => 1), step: 60, oom: { 10: 1 } }));
  const id = 'ns/deployment/api', at = T0 + 600;
  assert.strictEqual(wl.detail(data, model.compute(data, { range: [T0, at - 60] }), id).oom.count, 1);
  assert.strictEqual(wl.detail(data, model.compute(data, { range: [at, T0 + 1200] }), id).oom.count, 0);
});

test('waste bars are scaled to the whole list, not the page', () => {
  const rows = Array.from({ length: 60 }, (_, i) => ({
    id: 'ns/deployment/w' + i, name: 'w' + String(i).padStart(2, '0'), ns: 'ns', kind: 'deployment', status: 'ok', reason: '', notes: [],
    annualSpend: 100, annualSavings: 100 - i, savingsPct: (100 - i) / 100, wasteCores: 0, wasteGiB: 0, replicas: 1, oomKills: 0
  }));
  const ctx = ctxFor({ levers: model.defaultLevers });
  const second = wl.listParts(rows, { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 1 }, ctx);
  assert.match(second.table, /width:50%"><\/i><\/span>50%/);
});
