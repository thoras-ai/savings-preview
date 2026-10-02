'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');
const state = require('../src/js/state.js');
const prices = require('../src/js/prices.js');
const levers = require('../src/js/levers.js');
const ui = require('../src/js/components.js');

const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json'), 'utf8'));
const close = (a, b, tol = 1e-9) => Math.abs(a - b) <= tol * Math.max(1, Math.abs(b));

test('the new levers survive a round trip through the hash', () => {
  const st = Object.assign({}, state.DEFAULTS, {
    rates: { perCore: 0.0534, perGiB: 0.0134 }, node: 'gcp/europe-west1/n2-standard-4', discount: 0.3, bill: 250000
  });
  const hash = state.serialize(st);
  assert.strictEqual(hash, 'cpu=0.0534&gib=0.0134&node=gcp/europe-west1/n2-standard-4&discount=30&bill=250000');
  assert.deepStrictEqual(state.parse('#' + hash), st);
});

test('an instance type without rates, and discounts out of range, are dropped', () => {
  assert.strictEqual(state.parse('#node=aws/us-east-1/m5.xlarge').node, null);
  assert.strictEqual(state.parse('#cpu=1&gib=1&node=m5.xlarge').node, null);
  assert.strictEqual(state.parse('#discount=100').discount, 0);
  assert.strictEqual(state.parse('#discount=-5').discount, 0);
  assert.strictEqual(state.parse('#bill=0').bill, null);
  assert.strictEqual(state.levers(state.parse('#discount=25')).discount, 0.25);
});

test('the bill and the instance name change no figures; the discount does', () => {
  assert.strictEqual(state.affectsFigures(['bill', 'node']), false);
  assert.strictEqual(state.affectsFigures(['discount']), true);
});

test('a discount takes the same share off spend and savings and is named in the basis', () => {
  const data = model.prepare(fixture);
  const base = model.compute(data, state.levers(state.DEFAULTS));
  const off = model.compute(data, state.levers(Object.assign({}, state.DEFAULTS, { discount: 0.3 })));
  assert.ok(close(off.totals.spend, base.totals.spend * 0.7));
  assert.ok(close(off.totals.savings, base.totals.savings * 0.7));
  assert.strictEqual(off.totals.cores, base.totals.cores);
  assert.match(off.pricing.basis, /; 30% discount applied$/);
  assert.doesNotMatch(base.pricing.basis, /discount/);
});

// The section's figures come from model.compute with the levers in the
// state. At the defaults they must be analyze's totals, which the Go parity
// test holds to internal/savings.
test('at the defaults, and after resetting every lever, the section shows the Go reference totals', () => {
  const data = model.prepare(fixture);
  const reference = model.analyze(fixture).totals;
  assert.deepStrictEqual(model.compute(data, state.levers(state.parse(''))).totals, reference);

  const moved = Object.assign({}, state.DEFAULTS, {
    rates: { perCore: 0.05, perGiB: 0.006 }, node: 'aws/eu-west-1/m5.xlarge', discount: 0.2, headroom: 0.3, peakMinutes: 60,
    namespaces: [data.namespaces[0]], range: [data.fidelity.start + 86400, data.fidelity.end - 86400], engineerCost: 150000, bill: 1e6
  });
  model.compute(data, state.levers(moved));
  const reset = Object.assign({}, moved, levers.resetPatch(Object.keys(levers.LEVERS), state.DEFAULTS));
  assert.deepStrictEqual(reset, state.DEFAULTS);
  const r = model.compute(data, state.levers(reset));
  assert.deepStrictEqual(r.totals, reference);

  const shown = levers.figures(r, reset, ui, false);
  assert.deepStrictEqual(shown.map(f => f.label), ['Annual savings', 'Projected spend', 'Share of spend', 'CPU to free', 'Memory to free', 'Workloads counted']);
  assert.strictEqual(shown[0].value, ui.money(reference.savings));
  assert.strictEqual(shown[1].value, ui.money(reference.spend));
  assert.strictEqual(shown[5].value, reference.counted + ' of ' + reference.workloads);
});

test('provisional figures are rounded and marked, and a bill adds its share', () => {
  const r = { totals: { spend: 43091, savings: 12704, cores: 5.137, gib: 9.42, counted: 3, workloads: 4 } };
  const f = levers.figures(r, { bill: 250000 }, ui, true);
  assert.strictEqual(f[0].value, '~$13,000');
  assert.strictEqual(f[3].value, '~5.1 cores');
  assert.strictEqual(f[2].note, '5.1% of the cloud bill');
  assert.strictEqual(levers.figures(r, { bill: null }, ui, false)[2].note, '');
});

test('instance prices divide the hourly price by vCPUs and GiB, scaled by region', () => {
  const p = prices.lookup('aws', 'us-east-1', 'm5.xlarge');
  assert.deepStrictEqual([p.cores, p.gib, p.hourly, p.perCore, p.perGiB, p.scaled], [4, 16, 0.192, 0.048, 0.012, false]);
  const eu = prices.lookup('gcp', 'europe-west1', 'n2-standard-4');
  assert.ok(close(eu.hourly, 0.194236 * 1.1));
  assert.strictEqual(eu.scaled, true);
  assert.strictEqual(prices.lookup('aws', 'mars-1', 'm5.xlarge'), null);
  assert.strictEqual(prices.lookup('azure', 'eastus', 'm5.xlarge'), null);
  assert.strictEqual(prices.parseKey(prices.key(eu)).hourly, eu.hourly);
  assert.strictEqual(prices.parseKey('aws/us-east-1'), null);
  assert.strictEqual(prices.cloudOf('Standard_D4s_v5'), 'azure');
});

test('every catalog entry has positive specs and every region a factor of at least 1', () => {
  Object.keys(prices.CLOUDS).forEach(c => {
    const cloud = prices.CLOUDS[c];
    assert.strictEqual(cloud.regions[cloud.ref], 1, c);
    Object.values(cloud.regions).forEach(f => assert.ok(f >= 1, c));
    Object.entries(cloud.types).forEach(([t, s]) => assert.ok(s.length === 3 && s.every(v => v > 0), t));
    assert.strictEqual(prices.types(c).length, Object.keys(cloud.types).length);
  });
  assert.ok(Object.keys(prices.CLOUDS.gcp.types).length >= 20);
  assert.ok(Object.keys(prices.CLOUDS.azure.types).length >= 20);
});

test('the catalog agrees with the default AWS us-east-1 node pricing', () => {
  const nodes = ['m5.xlarge', 'c5.2xlarge', 'r5.large', 't3.medium'].map(t => {
    const s = prices.CLOUDS.aws.types[t];
    return { hostType: t, region: 'us-east-1', capacity: 'on_demand', cores: s[0], gib: s[1] };
  });
  const a = model.pricing(nodes), b = prices.fromNodes(nodes);
  assert.ok(close(a.perCore, b.perCore) && close(a.perGiB, b.perGiB));
});

test('nodes on other clouds and regions are priced by their own type and region', () => {
  const nodes = [
    { hostType: 'n2-standard-4', region: 'europe-west1', cores: 4, gib: 16 },
    { hostType: 'n2-standard-4', region: 'europe-west1', cores: 4, gib: 16 },
    { hostType: 'Standard_D4s_v5', region: 'nowhere', cores: 4, gib: 16 },
    { hostType: 'x9.unknown', region: 'us-east-1', cores: 4, gib: 16 }
  ];
  const r = prices.fromNodes(nodes);
  assert.strictEqual(r.priced, 3);
  assert.strictEqual(r.unknownRegion, 1);
  assert.ok(close(r.perCore, (2 * 0.194236 * 1.1 + 0.192) / 12));
  assert.deepStrictEqual(prices.guess(nodes), { cloud: 'gcp', region: 'europe-west1' });
  assert.strictEqual(prices.guess([{ hostType: 'x9.unknown' }]), null);
  assert.strictEqual(prices.fromNodes([]), null);
});

test('peak windows finer than the data step are disabled', () => {
  const off = w => levers.peakWindows(w.step, w.snapshot).filter(x => x.disabled).map(x => x.minutes);
  assert.deepStrictEqual(off({ step: 60 }), []);
  assert.deepStrictEqual(off({ step: 300 }), []);
  assert.deepStrictEqual(off({ step: 900 }), [5, 10]);
  assert.deepStrictEqual(off({ step: 3600 }), [5, 10, 15, 30]);
  assert.deepStrictEqual(off({ step: 60, snapshot: true }), levers.PEAK_WINDOWS);
  assert.strictEqual(levers.PEAK_WINDOWS[levers.peakIndex(15)], 15);
  assert.strictEqual(levers.PEAK_WINDOWS[levers.peakIndex(45)], 60);
  assert.strictEqual(levers.PEAK_WINDOWS[levers.peakIndex(1)], 5);
});

test('the time range offers whole days, whole hours or nothing, by span', () => {
  const day = 86400, t0 = 1789516800 + 3600;
  const days = levers.rangeChoices({ start: t0, end: t0 + 3 * day, snapshot: false }, null);
  assert.strictEqual(days.unit, 'day');
  assert.deepStrictEqual(days.from.map(o => o.label), ['Start of data', 'Sep 17', 'Sep 18', 'Sep 19']);
  assert.deepStrictEqual(days.to.map(o => o.label), ['Sep 17', 'Sep 18', 'Sep 19', 'End of data']);
  assert.strictEqual(levers.rangeFor(days, t0, t0 + 3 * day), null);
  assert.deepStrictEqual(levers.rangeFor(days, t0, days.to[0].value), [t0, days.to[0].value]);

  const hours = levers.rangeChoices({ start: t0, end: t0 + 5 * 3600, snapshot: false }, null);
  assert.strictEqual(hours.unit, 'hour');
  assert.strictEqual(hours.from[1].label, 'Sep 16, 02:00');
  assert.strictEqual(levers.rangeChoices({ start: t0, end: t0 + 3600, snapshot: false }, null), null);
  assert.strictEqual(levers.rangeChoices({ start: t0, end: t0, snapshot: true }, null), null);

  const linked = levers.rangeChoices({ start: t0, end: t0 + 3 * day, snapshot: false }, [t0 + 5400, t0 + 2 * day]);
  assert.ok(linked.from.some(o => o.value === t0 + 5400 && o.label === 'Sep 16, 02:30'));
});

test('namespace chips: all of them is no filter, and the last one stays on', () => {
  const all = ['a', 'b', 'c'];
  assert.deepStrictEqual(levers.toggleNamespace(null, all, 'b', false), ['a', 'c']);
  assert.strictEqual(levers.toggleNamespace(['a', 'c'], all, 'b', true), null);
  assert.deepStrictEqual(levers.toggleNamespace(['c'], all, 'a', true), ['a', 'c']);
  assert.deepStrictEqual(levers.toggleNamespace(['c'], all, 'c', false), ['c']);
});

test('each lever reports a change and resets alone', () => {
  const st = Object.assign({}, state.DEFAULTS, { rates: { perCore: 1, perGiB: 1 }, node: 'aws/us-east-1/m5.large', headroom: 0.2, section: 'money' });
  const c = levers.changed(st, state.DEFAULTS);
  assert.deepStrictEqual(Object.keys(c).filter(k => c[k]), ['headroom', 'rates']);
  assert.deepStrictEqual(levers.resetPatch(['rates'], state.DEFAULTS), { rates: null, node: null });
  assert.strictEqual(levers.anyChanged(Object.assign({}, state.DEFAULTS, { section: 'money', workload: 'x' }), state.DEFAULTS), false);
});

// A compact file's shape at the size the page is held to: 1,000 workloads
// over 14 days at 15-minute steps. Series objects are shared between
// workloads to keep the fixture small; the model copies each into its own arrays.
function largeDoc() {
  const step = 900, n = 14 * 86400 / step, t0 = 1789516800, GIB = 1 << 30;
  const S = (k, v) => ({ key: k, value: { stringValue: v } });
  const I = (k, v) => ({ key: k, value: { intValue: String(v) } });
  const metric = (name, f) => ({ name, gauge: { dataPoints: Array.from({ length: n }, (_, i) => ({ timeUnixNano: String((t0 + i * step) * 1e9), asDouble: f(i) })) } });
  const shapes = Array.from({ length: 16 }, (_, k) => [
    metric('container.cpu.usage', i => 0.2 + 0.15 * Math.sin((i + k * 7) / 96 * 2 * Math.PI) + (i % (37 + k) === 0 ? 0.3 : 0)),
    metric('container.memory.working_set', i => (1 + 0.3 * Math.sin((i + k) / 96 * 2 * Math.PI)) * GIB),
    metric('thoras.container.cpu.usage.max', i => 0.3 + 0.2 * Math.sin((i + k * 7) / 96 * 2 * Math.PI) + (i % (37 + k) === 0 ? 0.4 : 0)),
    metric('k8s.container.cpu.request.desired', () => 1 + k % 3),
    metric('k8s.container.memory.request.desired', () => (2 + k % 4) * GIB)
  ]);
  const replicas = [metric('k8s.deployment.pod.desired', () => 2)];
  const base = [S('k8s.cluster.name', 'large'), S('thoras.source', 'prometheus'), I('thoras.step_seconds', step), I('thoras.fidelity_tier', 2), I('thoras.source_step_seconds', 60)];
  const rms = [];
  for (let w = 0; w < 1000; w++) {
    const wl = base.concat([S('k8s.namespace.name', 'ns-' + (w % 25)), S('k8s.deployment.name', 'app-' + w)]);
    rms.push({ resource: { attributes: wl }, scopeMetrics: [{ metrics: replicas }] });
    rms.push({ resource: { attributes: wl.concat([S('k8s.container.name', 'app')]) }, scopeMetrics: [{ metrics: shapes[w % 16] }] });
  }
  return { resourceMetrics: rms };
}

test('a lever change on 1,000 workloads recomputes and renders the figures within 200 ms', () => {
  const data = model.prepare(largeDoc());
  assert.strictEqual(data.workloads.length, 1000);
  let st = Object.assign({}, state.DEFAULTS);
  model.compute(data, state.levers(st));
  const patches = [{ headroom: 0.2 }, { peakMinutes: 60 }, { discount: 0.3 }, { namespaces: data.namespaces.slice(1) }, { rates: { perCore: 0.05, perGiB: 0.006 } }];
  const times = patches.map(p => {
    st = Object.assign({}, st, p);
    const t = performance.now();
    const r = model.compute(data, state.levers(st));
    levers.figures(r, st, ui, false);
    levers.rangeChoices(r.fidelity, st.range);
    return performance.now() - t;
  }).sort((a, b) => a - b);
  const median = times[Math.floor(times.length / 2)];
  assert.ok(median < 200, 'median ' + median.toFixed(0) + ' ms, all ' + times.map(x => x.toFixed(0)).join(', '));
});
