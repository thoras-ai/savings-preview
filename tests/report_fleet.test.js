'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');
const files = require('../src/js/files.js');
const summary = require('../src/js/section-summary.js');
const state = require('../src/js/state.js');
const levers = require('../src/js/levers.js');
const prices = require('../src/js/prices.js');

const read = name => JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', name), 'utf8'));
const close = (a, b) => Math.abs(a - b) <= 1e-9 * Math.max(1, Math.abs(b));

// A copy of doc with every resource in cluster name, at the given contract version.
function named(doc, name, version) {
  const out = JSON.parse(JSON.stringify(doc));
  out.resourceMetrics.forEach(rm => {
    rm.resource.attributes.forEach(a => { if (a.key === 'k8s.cluster.name') a.value = { stringValue: name }; });
    (rm.scopeMetrics || []).forEach(sm => { if (version) sm.scope = Object.assign({}, sm.scope, { version }); });
  });
  return out;
}

function failed(name, reason) {
  return {
    resource: { attributes: [{ key: 'k8s.cluster.name', value: { stringValue: name } }, { key: 'thoras.cluster.error', value: { stringValue: reason } }] },
    schemaUrl: 'https://opentelemetry.io/schemas/1.44.0',
    scopeMetrics: [{ scope: { name: 'thoras.savings_preview', version: '1.4.0' }, schemaUrl: 'https://opentelemetry.io/schemas/1.44.0', metrics: [] }]
  };
}

const poll = read('minikube-poll.otlp.json');
const inter = read('intermittent.otlp.json');
const a = named(poll, 'alpha', '1.4.0'), b = named(inter, 'beta', '1.4.0');
const fleetDoc = { resourceMetrics: a.resourceMetrics.concat(b.resourceMetrics, [failed('gamma', 'list services: Unauthorized')]) };

test('fleet totals are the sum of the clusters, each priced from its own nodes', () => {
  const r = model.analyze(fleetDoc);
  const ra = model.analyze(named(poll, 'alpha')), rb = model.analyze(named(inter, 'beta'));
  assert.ok(r.fleet);
  assert.strictEqual(r.cluster, '2 clusters');
  for (const k of ['spend', 'savings', 'cores', 'gib', 'reqCores', 'reqGiB', 'useCores', 'useGiB', 'counted', 'workloads']) {
    assert.ok(close(r.totals[k], ra.totals[k] + rb.totals[k]), k + ': ' + r.totals[k]);
  }
  const by = Object.fromEntries(r.clusters.map(c => [c.name, c]));
  assert.ok(close(by.alpha.savings, ra.totals.savings) && close(by.beta.savings, rb.totals.savings));
  assert.ok(close(r.clusters.reduce((s, c) => s + c.spend, 0), r.totals.spend));
  // Sorted by savings, the biggest first.
  assert.ok(r.clusters[0].savings >= r.clusters[1].savings);
});

test('a fleet records the clusters that could not be read and qualifies names by cluster', () => {
  const data = model.prepare(fleetDoc);
  assert.deepStrictEqual(data.failed, [{ name: 'gamma', reason: 'list services: Unauthorized' }]);
  assert.deepStrictEqual(data.clusters.map(c => c.name), ['alpha', 'beta']);
  assert.strictEqual(data.contract.version, '1.4.0');
  assert.ok(data.workloads.every(w => w.id.startsWith(w.cluster + '/') && w.ns.startsWith(w.cluster + '/')));
  assert.throws(() => model.prepare({ resourceMetrics: [failed('gamma', 'expired')] }), /No cluster in this file could be read: gamma \(expired\)/);
});

test('the cluster filter gives the figures of the clusters it keeps', () => {
  const data = model.prepare(fleetDoc);
  const r = model.compute(data, { clusters: ['beta'] });
  const rb = model.analyze(named(inter, 'beta'));
  assert.strictEqual(r.fleet, false);
  assert.strictEqual(r.cluster, 'beta');
  assert.ok(close(r.totals.savings, rb.totals.savings) && close(r.totals.spend, rb.totals.spend));
  assert.strictEqual(r.pricing.basis, rb.pricing.basis);
  assert.strictEqual(r.nodes.length, model.prepare(named(inter, 'beta')).nodes.length);
});

test('a single-cluster file keeps its ids, namespaces and figures', () => {
  const data = model.prepare(poll);
  assert.strictEqual(data.multi, false);
  assert.ok(data.workloads.every(w => !w.id.startsWith(w.cluster + '/')));
  const r = model.compute(data);
  assert.strictEqual(r.fleet, false);
  assert.strictEqual(r.cluster, data.cluster);
  assert.strictEqual(r.pricing.basis, data.pricing.basis);
});

test('files from other contract versions are refused', () => {
  assert.throws(() => model.prepare(named(poll, 'x', '1.2.0')), /contract version 1\.2\.0 and this page reads only 1\.0\.0 and 1\.4\.0/);
});

test('combining files keeps the newer copy of a cluster and says which was left out', () => {
  const older = named(poll, 'alpha');
  const newer = named(poll, 'alpha');
  newer.resourceMetrics.forEach(rm => (rm.scopeMetrics || []).forEach(sm => (sm.metrics || []).forEach(m => {
    ((m.gauge || m.sum).dataPoints).forEach(p => { p.timeUnixNano = String(BigInt(p.timeUnixNano) + 3600000000000n); });
  })));
  const r = files.combine([{ name: 'new.json', doc: newer }, { name: 'old.json', doc: older }, { name: 'beta.json', doc: named(inter, 'beta') }]);
  assert.deepStrictEqual(r.dropped, [{ cluster: 'alpha', kept: 'new.json', dropped: 'old.json' }]);
  const data = model.prepare(r.doc, { dropped: r.dropped });
  assert.deepStrictEqual(data.clusters.map(c => c.name), ['alpha', 'beta']);
  assert.strictEqual(data.dropped.length, 1);
  // Counted once: the fleet is the newer alpha plus beta.
  const total = model.compute(data).totals.savings;
  assert.ok(close(total, model.analyze(newer).totals.savings + model.analyze(named(inter, 'beta')).totals.savings));

  // Equal data: the file chosen later wins. A file that only failed loses to data.
  const tie = files.combine([{ name: 'one.json', doc: older }, { name: 'two.json', doc: named(poll, 'alpha') }]);
  assert.deepStrictEqual(tie.dropped, [{ cluster: 'alpha', kept: 'two.json', dropped: 'one.json' }]);
  const fail = files.combine([{ name: 'data.json', doc: older }, { name: 'failed.json', doc: { resourceMetrics: [failed('alpha', 'expired')] } }]);
  assert.deepStrictEqual(fail.dropped, [{ cluster: 'alpha', kept: 'data.json', dropped: 'failed.json' }]);
});

test('files uploaded together show as one fleet with the summed figures', () => {
  const r = files.combine([{ name: 'a.json', doc: named(poll, 'alpha') }, { name: 'b.zip', doc: named(inter, 'beta') }]);
  assert.deepStrictEqual(r.dropped, []);
  const res = model.analyze(r.doc);
  assert.ok(res.fleet);
  assert.ok(close(res.totals.savings, model.analyze(named(poll, 'alpha')).totals.savings + model.analyze(named(inter, 'beta')).totals.savings));
  const one = files.combine([{ name: 'a.json', doc: poll }]);
  assert.strictEqual(one.doc, poll);
});

test('page one leads with the fleet total and lists the clusters', () => {
  const data = model.prepare(fleetDoc), st = state.parse('');
  const r = model.compute(data, state.levers(st));
  const html = summary.markup(data, r, st);
  assert.match(html, /Thoras would cut compute spend across your 2 clusters by \$/);
  assert.match(html, /Cluster by cluster/);
  assert.doesNotMatch(html, /Three workloads/);
  assert.doesNotMatch(html, /Clusters like this one/);
  assert.doesNotMatch(html, /gamma|not read|Unauthorized/);
  assert.match(html, /data-cluster="alpha"/);
  assert.match(summary.summaryText(r, st), /across 2 clusters/);

  // A link to one cluster shows that cluster as a single-cluster page one.
  const one = state.parse('#cluster=alpha');
  const ro = model.compute(data, state.levers(one));
  const h1 = summary.markup(data, ro, one);
  assert.match(h1, /Thoras would cut this cluster’s compute spend/);
  assert.match(h1, /Show all 2 clusters/);
  assert.strictEqual(state.serialize(one), 'cluster=alpha');
});

test('page one counts only the clusters with a workload in the figures', () => {
  const nodesOnly = named(poll, 'delta', '1.4.0').resourceMetrics.filter(rm => rm.resource.attributes.some(x => x.key === 'k8s.node.name'));
  const data = model.prepare({ resourceMetrics: fleetDoc.resourceMetrics.concat(nodesOnly) }), st = state.parse('');
  assert.strictEqual(data.clusters.length, 3);
  const r = model.compute(data, state.levers(st));
  const html = summary.markup(data, r, st);
  assert.match(html, /across your 2 clusters/);
  assert.match(html, /2 clusters, about/);
  assert.strictEqual((html.match(/data-cluster="/g) || []).length, 2);
  // With one counted cluster left in scope, page one is that cluster's.
  const one = state.parse('#cluster=alpha,delta');
  assert.strictEqual(model.compute(data, state.levers(one)).fleet, false);
});

test('the pricing card suggests node rates only for a single cluster', () => {
  const ref = read('savings-preview.otlp.json');
  const data = model.prepare({ resourceMetrics: fleetDoc.resourceMetrics.concat(named(ref, 'delta', '1.4.0').resourceMetrics) });
  assert.strictEqual(data.clusters.length, 3);
  const nodes = hash => levers.suggestionNodes(data, model.compute(data, state.levers(state.parse(hash))));
  // One rate for every cluster would replace their own pricing.
  assert.strictEqual(nodes(''), null);
  assert.strictEqual(nodes('#cluster=alpha,delta'), null);
  // A filter that keeps one cluster suggests that cluster's rates, without the others' nodes.
  const single = model.prepare(named(ref, 'delta'));
  assert.ok(nodes('#cluster=delta').length < data.nodes.length);
  assert.deepStrictEqual(nodes('#cluster=delta'), data.clusters.find(c => c.name === 'delta').nodes);
  assert.deepStrictEqual(prices.fromNodes(nodes('#cluster=delta')), prices.fromNodes(single.nodes));
  // A single-cluster file suggests from all of its nodes.
  assert.deepStrictEqual(levers.suggestionNodes(single, model.compute(single)), single.nodes);
});

const history = read('fleet-history.otlp.json');
const EAST = 'arn:aws:eks:us-east-1:111122223333:cluster/prod', WEST = 'arn:aws:eks:eu-west-1:111122223333:cluster/prod';
const GKE = 'gke_acme-data_us-central1_analytics';
const only = name => ({ resourceMetrics: history.resourceMetrics.filter(rm => rm.resource.attributes.some(x => x.key === 'k8s.cluster.name' && x.value.stringValue === name)) });

test('cluster names are shortened, and names that come out the same are told apart', () => {
  assert.deepStrictEqual(model.displayNames([EAST, GKE, 'staging']), { [EAST]: 'prod', [GKE]: 'analytics', staging: 'staging' });
  assert.deepStrictEqual(model.displayNames([EAST, WEST]), { [EAST]: 'prod (us-east-1)', [WEST]: 'prod (eu-west-1)' });
  const other = 'arn:aws:eks:us-east-1:444455556666:cluster/prod';
  assert.deepStrictEqual(model.displayNames([EAST, other]), { [EAST]: 'prod (111122223333)', [other]: 'prod (444455556666)' });
  assert.deepStrictEqual(model.displayNames([EAST, 'prod']), { [EAST]: 'prod (us-east-1)', prod: 'prod' });
  assert.deepStrictEqual(model.displayNames(['arn:aws-cn:eks:cn-north-1:1:cluster/a', 'gke_p_europe-west1-b_b']), { 'arn:aws-cn:eks:cn-north-1:1:cluster/a': 'a', 'gke_p_europe-west1-b_b': 'b' });
  assert.deepStrictEqual(model.displayNames(['arn:aws:eks:x', 'my_cluster']), { 'arn:aws:eks:x': 'arn:aws:eks:x', my_cluster: 'my_cluster' });

  const data = model.prepare(history);
  assert.deepStrictEqual(data.clusters.map(c => [c.name, c.label]), [[EAST, 'prod (us-east-1)'], [WEST, 'prod (eu-west-1)'], [GKE, 'analytics'], ['staging', 'staging']]);
  assert.strictEqual(model.prepare(only(EAST)).label, 'prod');
  assert.strictEqual(model.prepare(only(EAST)).cluster, EAST);
});

test('page one shows short cluster names and its links and filter keep the full ones', () => {
  const data = model.prepare(history), st = state.parse('');
  const r = model.compute(data, state.levers(st));
  const html = summary.markup(data, r, st);
  assert.match(html, />prod \(us-east-1\)</);
  assert.match(html, />analytics</);
  assert.match(html, new RegExp('data-cluster="' + EAST.replace(/[/.]/g, '\\$&') + '"'));
  assert.doesNotMatch(html, />arn:aws/);
  assert.match(summary.summaryText(r, st), /^ {2}prod \(us-east-1\): /m);

  // A link made before short names, naming the full identifier, still filters.
  const link = state.parse('#cluster=' + encodeURIComponent(EAST));
  const one = model.compute(data, state.levers(link));
  assert.strictEqual(one.cluster, EAST);
  assert.strictEqual(one.label, 'prod (us-east-1)');
  assert.ok(close(one.totals.savings, model.analyze(only(EAST)).totals.savings));
  assert.match(summary.method(link, summary.labelsOf(data)), /Only the cluster prod \(us-east-1\) is included\./);
});

test('a fleet charts the time every cluster covers and keeps each cluster’s own totals', () => {
  const data = model.prepare(history), r = model.compute(data);
  const starts = data.clusters.map(c => c.fidelity.start), ends = data.clusters.map(c => c.fidelity.end);
  assert.deepStrictEqual(r.window, [Math.max(...starts), Math.min(...ends)]);
  assert.strictEqual(r.window[1] - r.window[0], 6 * 3600);
  assert.ok(r.series.t.length >= 23);
  assert.ok(r.series.t[0] >= r.window[0] && r.series.t[r.series.t.length - 1] < r.window[1]);
  for (const f of ['cur', 'opt', 'useCost', 'reqCpu', 'tgtCpu']) assert.ok(r.series[f].every(Number.isFinite), f);

  // On its own, the longest cluster has a gap the fleet chart would have drawn blank.
  assert.ok(Array.from(model.analyze(only(EAST)).series.cur).some(v => !Number.isFinite(v)));

  // Totals cover each cluster's whole history.
  const alone = data.clusters.map(c => model.analyze(only(c.name)));
  for (const k of ['spend', 'savings', 'cores', 'gib', 'reqCores', 'reqGiB', 'counted', 'workloads']) {
    assert.ok(close(r.totals[k], alone.reduce((s, a) => s + a.totals[k], 0)), k);
  }
  for (const c of r.clusters) assert.ok(close(c.savings, model.analyze(only(c.name)).totals.savings), c.name);

  // A time range set under Assumptions is charted as it is, and a single cluster is not windowed.
  const ranged = model.compute(data, { range: [starts[0], ends[0]] });
  assert.strictEqual(ranged.window, null);
  assert.strictEqual(model.compute(data, { clusters: [EAST] }).window, null);
  assert.strictEqual(model.analyze(only(EAST)).window, null);
});

test('the fleet provenance line does not claim one cluster’s history for all of them', () => {
  const data = model.prepare(history), st = state.parse('');
  const r = model.compute(data, state.levers(st));
  assert.strictEqual(summary.provenance(r, st), 'From your Prometheus data across 4 clusters. Computed on your laptop. Nothing installed, nothing uploaded.');
  assert.strictEqual(summary.coverage(r, st), 'Sep 17 – Sep 18, 2026 (UTC)');
  const one = model.compute(model.prepare(only(EAST)));
  assert.match(summary.provenance(one, st), /^From 2 days of your Prometheus data\./);
  assert.strictEqual(summary.coverage(one, st), 'Sep 16 – Sep 18, 2026 (UTC)');
});

test('a fleet whose clusters end at different times charts no cliff at the end', () => {
  const doc = JSON.parse(JSON.stringify(history));
  const end = 1789689600 - 2 * 3600;
  doc.resourceMetrics.forEach(rm => {
    if (rm.resource.attributes.some(x => x.key === 'k8s.cluster.name' && x.value.stringValue === 'staging')) return;
    rm.scopeMetrics.forEach(sm => sm.metrics.forEach(m => {
      const body = m.gauge || m.sum;
      body.dataPoints = body.dataPoints.filter(p => Number(p.timeUnixNano) / 1e9 < end);
    }));
  });
  const r = model.analyze(doc), s = r.series;
  assert.deepStrictEqual(r.window, [1789689600 - 6 * 3600, end]);
  assert.ok(s.t[s.t.length - 1] < end);
  assert.ok(s.cur.every(Number.isFinite));
  const last = s.reqCpu[s.reqCpu.length - 1], before = s.reqCpu[s.reqCpu.length - 2];
  assert.ok(Math.abs(last - before) < 0.01 * before, last + ' after ' + before);
});
