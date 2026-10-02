'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

// The section scripts run against the page's globals, so load them the way
// the page does, with a registry that keeps what they register.
const registered = {};
globalThis.self = globalThis;
globalThis.SP = {
  model: require('../src/js/model.js'),
  state: require('../src/js/state.js'),
  ui: require('../src/js/components.js'),
  chart: require('../src/js/chart.js'),
  sections: { register: s => { registered[s.id] = s; } }
};
require('../src/js/section-summary.js');
require('../src/js/section-method.js');
require('../src/js/print.js');
const { model, state, ui } = SP;

function fixture(name = 'savings-preview.otlp.json') {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', name), 'utf8'));
}

function context(data, patch = {}) {
  const st = Object.assign({}, state.DEFAULTS, patch);
  const result = model.compute(data, state.levers(st));
  return { data, result, state: st, changed: null, approx: ui.confidenceLevel(result.fidelity) === 'provisional' };
}

function method(ctx) {
  const host = { innerHTML: '' };
  registered.method.render(host, ctx);
  return host.innerHTML;
}

const text = html => html.replace(/<[^>]+>/g, '').replace(/&amp;/g, '&').replace(/&#39;/g, "'");
const PRINTED = new Date(2026, 9, 2);

test('data and method describes the file at the default settings', () => {
  const out = text(method(context(model.prepare(fixture()))));
  for (const s of [
    'demo-cluster', 'prometheus', 'Sep 16, 12:00 AM to Sep 30, 12:00 AM UTC, 14 days', '5-minute samples',
    'Good confidence tier 2: at least 7 days at 5-minute resolution.', '3 counted of 4 in the file',
    '2 in the file, 2 with a known list price', '2 of 2 nodes at AWS us-east-1 on-demand list prices; spot nodes are priced at on-demand',
    'thoras.savings_preview 1.0.0, OpenTelemetry semantic conventions 1.44.0',
    'report-worker batch: missing memory usage (container.memory.working_set) data, so it is listed but not counted.',
    'ip-10-0-2-27 is a spot node priced at the on-demand list price.',
    'Every other required series was present for every workload. No fidelity notes were recorded.',
    'trailing 15-minute window. The target is 1.1 × that peak.', 'Average over the window'
  ]) assert.ok(out.includes(s), s);
});

test('data and method follows the levers', () => {
  const data = model.prepare(fixture());
  const t = model.compute(data).series.t;
  const out = text(method(context(data, {
    headroom: 0.2, peakMinutes: 30, namespaces: ['shop', 'batch'], rates: { perCore: 0.05, perGiB: 0.006 }, range: [t[0], t[288]]
  })));
  for (const s of [
    'trailing 30-minute window. The target is 1.2 × that peak.',
    '1 counted of 2 in 2 selected namespaces, 4 in the file',
    '$0.05 per core-hour and $0.006 per GiB-hour, set under Assumptions.',
    'at the rates set under Assumptions: $0.05 per core-hour and $0.006 per GiB-hour.',
    'Sep 16, 12:00 AM to Sep 17, 12:00 AM UTC, 1 day, the time range set under Assumptions. The file covers Sep 16',
    'Average over the time range set under Assumptions'
  ]) assert.ok(out.includes(s), s);
  assert.ok(!out.includes('ledger-db'), 'workloads outside the namespaces are not listed');
  assert.ok(!out.includes('is a spot node'), 'node pricing notes are left out when rates are set');
  const outside = text(method(context(data, { range: [1, 2] })));
  assert.ok(outside.includes('checkout-api shop: no data in the time range set under Assumptions'));
  assert.ok(outside.includes('No data in the time range set under Assumptions. The file covers Sep 16, 12:00 AM to Sep 30, 12:00 AM UTC, 14 days.'));
  assert.ok(!/-\d+ seconds/.test(outside));
  const outsidePrint = text(SP.print.html(context(data, { range: [1, 2] }), PRINTED));
  assert.ok(outsidePrint.includes('checkout-api (shop) has no data in the selected time range'), outsidePrint);
});

test('a polled file reads as provisional, with its rolled-up peaks named', () => {
  const ctx = context(model.prepare(fixture('minikube-poll.otlp.json')));
  assert.strictEqual(ctx.approx, true);
  const out = text(method(ctx));
  assert.ok(out.includes('Provisional tier 3'), out.slice(0, 400));
  assert.ok(out.includes('live polling'));
  const missed = text(method(context(ctx.data, { range: [1, 2] })));
  assert.ok(missed.includes('No data in the time range set under Assumptions. The file covers'), missed.slice(0, 600));
  const printed = SP.print.html(ctx, PRINTED);
  assert.ok(printed.includes('banner-caution'));
  assert.ok(printed.includes('print-provisional'));
  assert.ok(text(printed).includes(ui.money(ctx.result.totals.savings, true)));
  assert.match(ui.money(ctx.result.totals.savings, true), /^~/);
});

test('the printed summary shows the totals the parity test checks against the Go figures', () => {
  const doc = fixture();
  const go = model.analyze(doc).totals;
  const out = text(SP.print.html(context(model.prepare(doc)), PRINTED));
  for (const s of [
    'printed Oct 2, 2026', ui.money(go.savings), ui.money(go.spend), ui.money(go.spend - go.savings), ui.money(go.savings / 12),
    ui.num(go.cores, 1) + ' cores', ui.num(go.gib, 1) + ' GiB', ui.pct(go.savings / go.spend) + ' of projected spend',
    '1 of 4 workloads has too little data for a savings figure and is not in the totals: report-worker (batch) is missing memory usage data.',
    'priced from the node inventory (2 of 2 nodes at AWS us-east-1 on-demand list prices; spot nodes are priced at on-demand), averaged',
    'Target is 1.1 times the highest usage in any 15-minute window'
  ]) assert.ok(out.includes(s), s);
  assert.deepStrictEqual(out.match(/checkout-api|ledger-db|search-indexer/g), ['checkout-api', 'ledger-db', 'search-indexer']);
});

test('the printed summary follows the levers', () => {
  const data = model.prepare(fixture());
  const ctx = context(data, { headroom: 0.3, namespaces: ['shop'], engineerCost: 100000 });
  const out = text(SP.print.html(ctx, PRINTED));
  assert.ok(out.includes(ui.money(ctx.result.totals.savings)));
  assert.notStrictEqual(ctx.result.totals.savings, model.analyze(fixture()).totals.savings);
  assert.ok(text(SP.print.html(context(data, { rates: { perCore: 0.05, perGiB: 0.006 } }), PRINTED)).includes('priced at $0.05 per core-hour and $0.006 per GiB-hour, averaged'));
  for (const s of ['Filtered', 'Target is 1.3 times', 'Only the namespaces shop are counted.', 'at $100,000 loaded cost']) assert.ok(out.includes(s), s);
});

// A compact file at the scale the page must stay fast for: 1,000 workloads,
// 14 days at 15-minute steps. Points are shared between resources, which
// model.prepare copies anyway, so the file builds quickly.
function largeDoc() {
  const step = 900, n = 14 * 96, t0 = 1789516800, GIB = 2 ** 30;
  const S = (k, v) => ({ key: k, value: { stringValue: v } });
  const I = (k, v) => ({ key: k, value: { intValue: String(v) } });
  const pts = f => Array.from({ length: n }, (_, i) => ({ timeUnixNano: String((t0 + i * step) * 1e9), asDouble: f(i) }));
  const cpu = Array.from({ length: 5 }, (_, k) => pts(i => 0.1 + 0.05 * k + 0.05 * Math.sin(i / 16 + k)));
  const mem = Array.from({ length: 5 }, (_, k) => pts(i => (0.5 + 0.2 * k + 0.1 * Math.cos(i / 16)) * GIB));
  const req = [0.25, 0.5, 1, 2].map(v => pts(() => v));
  const memReq = [1, 2, 4].map(v => pts(() => v * GIB));
  const one = pts(() => 1);
  const base = [S('k8s.cluster.name', 'large'), S('thoras.source', 'prometheus'), I('thoras.step_seconds', step), I('thoras.fidelity_tier', 1), I('thoras.source_step_seconds', 60)];
  const rms = [];
  for (let w = 0; w < 1000; w++) {
    const wl = base.concat([S('k8s.namespace.name', 'ns-' + (w % 40)), S('k8s.deployment.name', 'app-' + w)]);
    rms.push({ resource: { attributes: wl }, scopeMetrics: [{ metrics: [{ name: 'k8s.deployment.pod.desired', sum: { dataPoints: one } }] }] });
    const metrics = [
      { name: 'container.cpu.usage', gauge: { dataPoints: cpu[w % 5] } },
      { name: 'thoras.container.cpu.usage.max', gauge: { dataPoints: cpu[(w + 1) % 5] } },
      { name: 'container.memory.working_set', sum: { dataPoints: mem[w % 5] } },
      { name: 'k8s.container.cpu.request.desired', sum: { dataPoints: req[w % 4] } },
      { name: 'k8s.container.memory.request.desired', sum: { dataPoints: memReq[w % 3] } }
    ].filter(m => w % 50 !== 7 || m.name !== 'container.memory.working_set');
    rms.push({ resource: { attributes: wl.concat([S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics }] });
  }
  for (let k = 0; k < 60; k++) {
    rms.push({
      resource: { attributes: base.concat([S('k8s.node.name', 'node-' + k), S('host.type', 'm5.2xlarge'), S('cloud.region', 'us-east-1'), S('thoras.node.capacity_type', k % 3 ? 'on_demand' : 'spot')]) },
      scopeMetrics: [{ metrics: [{ name: 'k8s.node.cpu.allocatable', sum: { dataPoints: pts(() => 8) } }, { name: 'k8s.node.memory.allocatable', sum: { dataPoints: pts(() => 32 * GIB) } }] }]
    });
  }
  return { resourceMetrics: rms };
}

test('data and method and the printed summary render 1,000 workloads within the lever budget', () => {
  const data = model.prepare(largeDoc());
  const ctx = context(data);
  assert.strictEqual(ctx.result.totals.workloads, 1000);
  assert.strictEqual(ctx.result.totals.counted, 980);
  let html = '', printed = '';
  const t0 = performance.now();
  for (let i = 0; i < 5; i++) { html = method(ctx); printed = SP.print.html(ctx, PRINTED); }
  const each = (performance.now() - t0) / 5;
  // A lever change has 200 ms for every section; these two get a small share of it.
  assert.ok(each < 50, each.toFixed(1) + ' ms');
  const out = text(html);
  assert.ok(out.includes('1-minute samples, kept as 15-minute peaks and means'));
  assert.ok(out.includes('Show 32 more'), 'the 20 workloads and 20 spot nodes past the first 8 fold away');
  assert.strictEqual((html.match(/<li>/g) || []).length, 40 + 6 + 4);
  assert.ok(text(printed).includes('20 of 1,000 workloads have too little data'));
  assert.ok(text(printed).includes('; and 17 more.'));
});
