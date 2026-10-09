'use strict';
// The engineer sections of a fleet report break their figures down by cluster.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

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
const wl = require('../src/js/section-workloads.js');
const money = require('../src/js/section-money.js');
const cluster = require('../src/js/section-cluster.js');

const read = name => JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', name), 'utf8'));

function named(doc, name) {
  const out = JSON.parse(JSON.stringify(doc));
  out.resourceMetrics.forEach(rm => {
    rm.resource.attributes.forEach(a => { if (a.key === 'k8s.cluster.name') a.value = { stringValue: name }; });
    (rm.scopeMetrics || []).forEach(sm => { sm.scope = Object.assign({}, sm.scope, { version: '1.4.0' }); });
  });
  return out;
}

// Three copies of one cluster, so every namespace name is in each of them,
// and a cluster with other namespaces, other history and no nodes.
const ALPHA = 'arn:aws:eks:us-east-1:111122223333:cluster/alpha';
const BETA = 'arn:aws:eks:eu-west-1:111122223333:cluster/beta';
const DELTA = 'arn:aws:eks:us-west-2:444455556666:cluster/delta';
const GAMMA = 'gke_shop-project_us-central1_gamma';
const poll = read('minikube-poll.otlp.json'), inter = read('intermittent.otlp.json');
const fleetDoc = { resourceMetrics: [ALPHA, BETA, DELTA].flatMap(n => named(poll, n).resourceMetrics).concat(named(inter, GAMMA).resourceMetrics) };
const data = model.prepare(fleetDoc);

function context(patch = {}) {
  const st = Object.assign({}, state.DEFAULTS, patch);
  return { data, result: model.compute(data, state.levers(st)), state: st, changed: null, approx: false };
}

const VIEW = { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0, mode: 'cpu', last: null };
const MAP = { level: null, metric: 'savings', cluster: null, ns: null, allNs: false };
const text = html => html.replace(/<[^>]+>/g, ' ').replace(/&amp;/g, '&').replace(/&#39;/g, "'").replace(/\s+/g, ' ');

test('each cluster in a fleet has the figures it has when filtered alone', () => {
  for (const patch of [{}, { discount: 0.2 }, { rates: { perCore: 0.03, perGiB: 0.004 } }, { headroom: 0.3, peakMinutes: 60 }]) {
    const ctx = context(patch);
    for (const name of [ALPHA, BETA, DELTA, GAMMA]) {
      const alone = model.compute(data, state.levers(Object.assign({}, ctx.state, { onlyClusters: [name] })));
      const part = model.forCluster(data, ctx.result, name);
      assert.deepStrictEqual(part.totals, alone.totals, name);
      assert.deepStrictEqual(part.workloads.map(w => w.id), alone.workloads.map(w => w.id));
      for (const k of ['cluster', 'label', 'fidelity', 'pricing', 'basePricing', 'nodes']) assert.deepStrictEqual(part[k], alone[k], name + ' ' + k);
    }
  }
});

test('Cluster and nodes shows each cluster first, each as it shows filtered alone', () => {
  const ctx = context({ namespaces: null });
  const list = cluster.byCluster(data, ctx.result, ctx.state);
  assert.deepStrictEqual(list.map(c => c.label), ['alpha', 'beta', 'delta', 'gamma']);
  for (const c of list) {
    const st = Object.assign({}, ctx.state, { onlyClusters: [c.name] });
    const alone = model.compute(data, state.levers(st));
    assert.deepStrictEqual(c.s, cluster.summarize(data, alone, st), c.label);
    // Filtered alone, the section shows no breakdown.
    assert.strictEqual(cluster.byCluster(data, alone, st), null);
  }
  // Each cluster is averaged over its own window, so its capacity is its own.
  assert.ok(list[0].s.alloc && list[0].s.alloc.cores > 0);
  assert.strictEqual(list[3].s.alloc, null);
  const html = cluster.markup(cluster.summarize(data, ctx.result, ctx.state), false, list);
  assert.ok(html.indexOf('By cluster') < html.indexOf('Fleet total'));
  assert.match(html, new RegExp('title="' + ALPHA + '">alpha<'));
  assert.match(text(html), /No nodes/);
});

test('Where the money is keys namespaces by cluster, so names in two clusters stay apart', () => {
  const r = context().result;
  const sum = money.summarize(r);
  const kube = sum.namespaces.filter(n => n.nsName === 'kube-system');
  assert.strictEqual(kube.length, 3);
  assert.deepStrictEqual(kube.map(n => n.display).sort(), ['alpha / kube-system', 'beta / kube-system', 'delta / kube-system']);
  assert.strictEqual(sum.namespaces.length, 3 * 6 + 2);
  // A namespace has the figures of the same namespace in the cluster alone.
  const alone = money.summarize(model.compute(data, { clusters: [BETA] }));
  const beta = alone.namespaces.find(n => n.nsName === 'kube-system');
  assert.strictEqual(kube.find(n => n.cluster === BETA).savings, beta.savings);
  assert.strictEqual(beta.display, 'kube-system');
  // Clusters add up to the fleet.
  assert.deepStrictEqual(sum.clusters.map(c => c.name).sort(), [ALPHA, BETA, DELTA, GAMMA].sort());
  const total = sum.clusters.reduce((s, c) => s + c.savings, 0);
  assert.ok(Math.abs(total - r.totals.savings) < 1e-6);
  // Within one cluster's view, only its namespaces, named without the cluster.
  const one = money.summarize(r, GAMMA);
  assert.deepStrictEqual(one.namespaces.map(n => n.display).sort(), ['monitoring', 'shop']);
  assert.strictEqual(one.counted + one.uncounted, sum.clusters.find(c => c.name === GAMMA).workloads);
});

test('the map opens on clusters in a large fleet, then a cluster opens on its namespaces', () => {
  const r = context().result;
  assert.ok(r.workloads.length > money.MAP_WORKLOADS);
  const top = money.html(r, state.DEFAULTS, MAP, false, 1100);
  assert.match(top, /Savings by cluster/);
  assert.match(top, /data-cluster="arn:aws:eks:us-east-1:111122223333:cluster\/alpha"/);
  assert.match(top, /<h2>By cluster<\/h2>/);
  assert.match(text(top), /With more than 60 workloads the map opens at the cluster level/);
  const drill = money.html(r, state.DEFAULTS, Object.assign({}, MAP, { cluster: ALPHA }), false, 1100);
  assert.match(drill, /Savings in alpha by namespace/);
  assert.match(drill, /data-map-back="clusters"/);
  assert.doesNotMatch(drill, new RegExp('data-ns="' + BETA));
  const ns = money.html(r, state.DEFAULTS, Object.assign({}, MAP, { cluster: ALPHA, ns: ALPHA + '/kube-system' }), false, 1100);
  assert.match(ns, /Savings in alpha \/ kube-system/);
  assert.match(ns, /data-map-back="ns"/);
  // Namespaces across the fleet are named with their cluster.
  const all = money.html(r, state.DEFAULTS, Object.assign({}, MAP, { level: 'namespaces' }), false, 1100);
  assert.match(all, /<b>gamma \/ shop<\/b>/);
  // The Clusters filter leaves one cluster: no cluster level and no cluster ranking.
  const filtered = model.compute(data, { clusters: [GAMMA] });
  const html = money.html(filtered, Object.assign({}, state.DEFAULTS, { onlyClusters: [GAMMA] }), Object.assign({}, MAP, { cluster: ALPHA }), false, 1100);
  assert.doesNotMatch(html, /By cluster|data-level="clusters"|data-cluster=/);
});

test('Workloads lists the cluster in a column, and sorting by it groups each cluster', () => {
  const ctx = context();
  const rows = wl.rows(data, ctx.result);
  const parts = wl.listParts(rows, Object.assign({}, VIEW), ctx);
  assert.match(parts.table, /data-sort="cluster"/);
  assert.doesNotMatch(parts.table, /class="wl-sub">arn:/);
  assert.match(parts.table, new RegExp('<td><span title="' + ALPHA + '">alpha</span></td>'));
  assert.strictEqual(parts.filter, 'Filter by name, namespace or cluster');
  const view = Object.assign({}, VIEW, { sort: { key: 'cluster', dir: 1 } });
  const list = wl.arrange(rows, view);
  const order = list.map(r => r.clusterLabel).filter((l, i, a) => a.indexOf(l) === i);
  assert.deepStrictEqual(order, ['alpha', 'beta', 'delta', 'gamma']);
  // Within a cluster, counted workloads by savings, then the rest.
  for (const name of [ALPHA, GAMMA]) {
    const block = list.filter(r => r.cluster === name);
    const counted = block.filter(r => r.status !== 'excluded').length;
    assert.ok(block.slice(counted).every(r => r.status === 'excluded'), name);
    for (let i = 1; i < counted; i++) assert.ok(block[i - 1].annualSavings >= block[i].annualSavings, name);
  }
  const made = [['a', 'x', 'excluded', 0], ['b', 'x', 'ok', 5], ['c', 'w', 'ok', 1], ['d', 'x', 'ok', 9]].map(([name, label, status, annualSavings]) =>
    ({ name, ns: 'ns', nsName: 'ns', cluster: label, clusterLabel: label, status, annualSavings }));
  assert.deepStrictEqual(wl.arrange(made, view).map(r => r.name), ['c', 'd', 'b', 'a']);
  const grouped = wl.listParts(rows, view, ctx);
  assert.strictEqual((grouped.table.match(/class="wl-group"/g) || []).length, 3);
  // A cluster that runs onto the next page repeats its heading there.
  const next = wl.listParts(rows, Object.assign({}, view, { page: 1 }), ctx);
  assert.deepStrictEqual((next.table.match(/class="wl-group"><th[^>]*><span title="[^"]*">\w+/g) || []).map(h => h.split('>').pop()), ['delta', 'gamma']);
  assert.match(text(grouped.table), /alpha 21 workloads/);
  // The filter matches the cluster's short name.
  assert.ok(wl.arrange(rows, Object.assign({}, VIEW, { filter: 'gamma' })).every(r => r.cluster === GAMMA));
  // And its full name, so a region or account finds the cluster.
  const west = wl.arrange(rows, Object.assign({}, VIEW, { filter: 'us-west-2' }));
  assert.ok(west.length && west.every(r => r.cluster === DELTA));
  // A single-cluster report does not match its cluster's name.
  const single = model.prepare(poll);
  const local = wl.rows(single, model.compute(single, state.levers(state.DEFAULTS)));
  assert.strictEqual(wl.arrange(local, Object.assign({}, VIEW, { filter: single.cluster })).length, 0);
  assert.strictEqual(wl.arrange(local, Object.assign({}, VIEW, { filter: 'team-1' })).length, local.filter(r => r.ns === 'team-1').length);
  // Filtered to one cluster, the column goes and a cluster sort falls back to savings.
  const one = context({ onlyClusters: [GAMMA] });
  const v = Object.assign({}, view);
  const solo = wl.listParts(wl.rows(data, one.result), v, one);
  assert.doesNotMatch(solo.table, /data-sort="cluster"|wl-group/);
  assert.strictEqual(v.sort.key, 'annualSavings');
});

test('the drill-down names the cluster, with its full name on hover', () => {
  const ctx = context();
  const w = ctx.result.workloads.find(x => x.cluster === BETA && x.series);
  const html = wl.detailHtml(wl.detail(data, ctx.result, w.id), VIEW, ctx);
  assert.match(html, new RegExp('<div class="sub"><span title="' + BETA + '">beta</span> · ' + w.nsName + ' · '));
});

test('Data and method names clusters by short name, with the full name under it', () => {
  const ctx = context();
  const host = { innerHTML: '' };
  registered.method.render(host, ctx);
  assert.match(host.innerHTML, new RegExp('title="' + GAMMA + '">gamma</span><div class="mono-sub">' + GAMMA + '</div>'));
  assert.doesNotMatch(host.innerHTML, /<span class="mono-sub">arn:/);
});
