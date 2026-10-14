'use strict';
// Thoras's own namespaces, thoras and thoras-*, are left out of every figure
// unless the includeThoras lever is set.
const test = require('node:test');
const assert = require('node:assert');

const registered = {};
globalThis.self = globalThis;
globalThis.SP = {
  model: require('../src/js/model.js'),
  state: require('../src/js/state.js'),
  ui: require('../src/js/components.js'),
  chart: require('../src/js/chart.js'),
  sections: { register: s => { registered[s.id] = s; } }
};
SP.summary = require('../src/js/section-summary.js');
require('../src/js/section-method.js');
const { model, state } = SP;
const levers = require('../src/js/levers.js');

const GIB = 1 << 30;
const S = (k, v) => ({ key: k, value: { stringValue: v } });
const I = (k, v) => ({ key: k, value: { intValue: String(v) } });

function series(name, values, opts = {}) {
  const pts = [];
  values.forEach((v, i) => {
    if (v === null) return;
    const p = { timeUnixNano: String((1789516800 + i * 300) * 1e9) };
    if (opts.int) p.asInt = String(v); else p.asDouble = v;
    pts.push(p);
  });
  return { name, sum: { dataPoints: pts } };
}

// The same workloads as thorasDoc in internal/savings/savings_test.go: 20
// steps; shop/app and thorasbank/ledger run the last 10, thoras and
// thoras-console run all 20.
function doc(cluster = 'test-cluster') {
  const rms = [];
  [['shop', 'app', 10, 2], ['thorasbank', 'ledger', 10, 1], ['thoras', 'metrics-collector', 0, 0.5], ['thoras-console', 'console', 0, 0.25]].forEach(([ns, name, from, req]) => {
    const base = [S('k8s.cluster.name', cluster), S('thoras.source', 'prometheus'), I('thoras.step_seconds', 300), I('thoras.fidelity_tier', 2), S('k8s.namespace.name', ns), S('k8s.deployment.name', name)];
    const at = v => Array.from({ length: 20 }, (_, i) => (i < from ? null : v));
    rms.push({ resource: { attributes: base }, scopeMetrics: [{ metrics: [series('k8s.deployment.pod.desired', Array(20).fill(1), { int: true })] }] });
    rms.push({ resource: { attributes: base.concat([S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics: [
      series('container.cpu.usage', at(0.1)), series('container.memory.working_set', at(GIB / 4), { int: true }),
      series('k8s.container.cpu.request.desired', at(req)), series('k8s.container.memory.request.desired', at(GIB), { int: true })
    ] }] });
  });
  return { resourceMetrics: rms };
}

const close = (a, b) => Math.abs(a - b) < 1e-9;

test('Thoras namespaces are left out of totals, workloads and the chart by default', () => {
  const data = model.prepare(doc());
  assert.deepStrictEqual(data.thorasNamespaces, ['thoras', 'thoras-console']);
  const r = model.compute(data, model.defaultLevers);
  assert.deepStrictEqual(r.workloads.map(w => w.ns).sort(), ['shop', 'thorasbank']);
  assert.strictEqual(r.totals.workloads, 2);
  // The customer's workloads alone cover the last 10 steps, so they count in full.
  assert.ok(close(r.totals.reqCores, 3), String(r.totals.reqCores));
  // The chart starts where the counted workloads do.
  assert.strictEqual(r.series.t.length, 10);
  assert.ok(Array.from(r.series.reqCpu).every(v => v === 3));
});

test('the lever counts them, and the cluster time they cover', () => {
  const data = model.prepare(doc());
  const r = model.compute(data, { includeThoras: true });
  assert.strictEqual(r.totals.workloads, 4);
  assert.ok(close(r.totals.reqCores, 3 * 0.5 + 0.5 + 0.25), String(r.totals.reqCores));
  assert.strictEqual(r.series.t.length, 20);
  assert.ok(close(r.series.reqCpu[0], 0.75) && close(r.series.reqCpu[19], 3.75));
  // Switching back gives the defaults' figures again.
  assert.ok(close(model.compute(data, model.defaultLevers).totals.reqCores, 3));
});

test('in a fleet Thoras namespaces are named by cluster and left out of each', () => {
  const a = doc('a'), b = doc('b');
  const data = model.prepare({ resourceMetrics: a.resourceMetrics.concat(b.resourceMetrics) });
  assert.deepStrictEqual(data.thorasNamespaces, ['a/thoras', 'a/thoras-console', 'b/thoras', 'b/thoras-console']);
  const r = model.compute(data, model.defaultLevers);
  assert.strictEqual(r.totals.workloads, 4);
  assert.ok(close(r.totals.reqCores, 6));
});

test('the link keeps the lever, and the Namespaces filter does not offer them while left out', () => {
  const st = Object.assign({}, state.DEFAULTS, { includeThoras: true });
  assert.strictEqual(state.serialize(st), 'thoras=1');
  assert.deepStrictEqual(state.parse('#thoras=1'), st);
  assert.strictEqual(state.levers(state.DEFAULTS).includeThoras, false);
  assert.strictEqual(state.levers(st).includeThoras, true);
  assert.ok(levers.changed(st, state.DEFAULTS).thoras);
  assert.deepStrictEqual(levers.resetPatch(['thoras'], state.DEFAULTS), { includeThoras: false });
  const data = model.prepare(doc());
  assert.deepStrictEqual(levers.shownNamespaces(data, state.DEFAULTS), ['shop', 'thorasbank']);
  assert.deepStrictEqual(levers.shownNamespaces(data, st), data.namespaces);
});

test('the page says they are left out, and says nothing when there are none', () => {
  const data = model.prepare(doc());
  const ctx = patch => {
    const st = Object.assign({}, state.DEFAULTS, patch);
    return { data, result: model.compute(data, state.levers(st)), state: st, changed: null };
  };
  const method = c => { const host = { innerHTML: '' }; registered.method.render(host, c); return host.innerHTML; };
  const note = 'Thoras’s own namespaces (thoras, thoras-console) are left out.';
  assert.ok(method(ctx({})).includes('2 counted of 4 in the file. ' + note));
  assert.ok(SP.summary.method(state.DEFAULTS, null, model.thorasLeftOut(data, false)).endsWith(' ' + note));
  assert.ok(!method(ctx({ includeThoras: true })).includes('Thoras’s own'));
  assert.deepStrictEqual(model.thorasLeftOut(data, true), []);
  const plain = model.prepare({ resourceMetrics: doc().resourceMetrics.filter(rm => !rm.resource.attributes.some(x => x.key === 'k8s.namespace.name' && /^thoras(-|$)/.test(x.value.stringValue))) });
  assert.deepStrictEqual(plain.thorasNamespaces, []);
  assert.ok(!method({ data: plain, result: model.compute(plain), state: state.DEFAULTS, changed: null }).includes('Thoras’s own'));
  assert.strictEqual(SP.ui.thorasNote(['thoras']), 'Thoras’s own namespace (thoras) is left out.');
});
