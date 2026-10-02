'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const model = require('../src/js/model.js');
const money = require('../src/js/section-money.js');
const state = require('../src/js/state.js');
const { budget } = require('./budget');

const VIEW = { level: null, metric: 'savings', ns: null, allNs: false };

function fixture() {
  return model.prepare(JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', 'savings-preview.otlp.json'), 'utf8')));
}

function close(a, b) { return Math.abs(a - b) <= 1e-9 * Math.max(1, Math.abs(a), Math.abs(b)); }

// A computed result with n workloads over 20 namespaces, every 21st one
// without data, as compute() returns it.
function large(n) {
  const workloads = [];
  for (let i = 0; i < n; i++) {
    const spend = 2000 + (i * 7919) % 9000, counted = i % 21 !== 7;
    workloads.push({
      id: `ns-${i % 20}/deployment/app-${i}`, ns: `ns-${i % 20}`, kind: 'deployment', name: `app-${i}`,
      series: counted ? {} : null, status: counted ? 'ok' : 'excluded', reason: counted ? '' : 'Missing memory usage data',
      annualSpend: counted ? spend : 0, annualSavings: counted ? spend * ((i * 31) % 80) / 100 : 0, savingsPct: counted ? ((i * 31) % 80) / 100 : 0
    });
  }
  const totals = { spend: 0, savings: 0, workloads: n, counted: 0 };
  workloads.forEach(w => { if (w.series) { totals.counted++; totals.spend += w.annualSpend; totals.savings += w.annualSavings; } });
  return { workloads, totals };
}

test('namespace figures at the default levers add up to the totals the Go math matches', () => {
  const r = model.compute(fixture(), state.levers(state.DEFAULTS));
  const sum = money.summarize(r);
  let savings = 0, spend = 0;
  sum.namespaces.forEach(n => { savings += n.savings; spend += n.spend; });
  assert.ok(close(savings, r.totals.savings), `${savings} vs ${r.totals.savings}`);
  assert.ok(close(spend, r.totals.spend), `${spend} vs ${r.totals.spend}`);
  assert.strictEqual(sum.savings, r.totals.savings);
  assert.strictEqual(sum.counted, 3);
  assert.strictEqual(sum.uncounted, 1);
  const shop = sum.namespaces.find(n => n.name === 'shop');
  assert.strictEqual(shop.savings, r.workloads.find(w => w.ns === 'shop').annualSavings);
});

test('namespaces rank by savings, take the four chart colours, and uncounted ones go last', () => {
  const sum = money.summarize(model.compute(fixture()));
  assert.deepStrictEqual(sum.namespaces.map(n => [n.name, n.tone]), [['shop', '1'], ['data', '2'], ['search', '3'], ['batch', 'other']]);
  assert.strictEqual(sum.namespaces[3].counted, 0);
  assert.match(sum.namespaces[3].reason, /memory usage/);
  const big = money.summarize(large(1000));
  assert.deepStrictEqual(big.namespaces.slice(0, 5).map(n => n.tone), ['1', '2', '3', '4', 'other']);
  for (let i = 1; i < big.namespaces.length; i++) assert.ok(big.namespaces[i - 1].savings >= big.namespaces[i].savings);
});

test('the section reacts to the levers', () => {
  const data = fixture();
  const base = model.compute(data);
  const html = money.html(base, state.DEFAULTS, VIEW, false, 1100);
  const more = model.compute(data, state.levers(Object.assign({}, state.DEFAULTS, { headroom: 0.5 })));
  assert.ok(more.totals.savings < base.totals.savings);
  assert.notStrictEqual(money.html(more, state.DEFAULTS, VIEW, false, 1100), html);
  const st = Object.assign({}, state.DEFAULTS, { namespaces: ['shop'] });
  const shop = money.html(model.compute(data, state.levers(st)), st, VIEW, false, 1100);
  assert.match(shop, /Filtered/);
  assert.doesNotMatch(shop, /ledger-db/);
  assert.match(shop, /across 1 counted workload\./);
});

test('treemap cells fill the map without overlapping, in proportion to their values', () => {
  const items = [6, 6, 4, 3, 2, 2, 1].map((value, i) => ({ value, i }));
  const W = 600, H = 400, rects = money.squarify(items, W, H);
  assert.strictEqual(rects.length, items.length);
  let area = 0;
  rects.forEach(r => {
    area += r.w * r.h;
    assert.ok(close(r.w * r.h, r.item.value / 24 * W * H), `cell ${r.item.i}`);
    assert.ok(r.x >= -1e-9 && r.y >= -1e-9 && r.x + r.w <= W + 1e-6 && r.y + r.h <= H + 1e-6);
  });
  assert.ok(close(area, W * H));
  for (let a = 0; a < rects.length; a++) {
    for (let b = a + 1; b < rects.length; b++) {
      const p = rects[a], q = rects[b];
      const ox = Math.min(p.x + p.w, q.x + q.w) - Math.max(p.x, q.x), oy = Math.min(p.y + p.h, q.y + q.h) - Math.max(p.y, q.y);
      assert.ok(ox <= 1e-6 || oy <= 1e-6, `cells ${a} and ${b} overlap`);
    }
  }
  // Squarified cells stay close to square.
  rects.slice(0, 3).forEach(r => assert.ok(Math.max(r.w / r.h, r.h / r.w) < 3));
  assert.deepStrictEqual(money.squarify([], W, H), []);
});

test('the map shows workloads in a small file, with a hatched cell for the uncounted one', () => {
  const r = model.compute(fixture());
  const cells = money.cells(r, money.summarize(r), VIEW, false);
  assert.deepStrictEqual(cells.map(c => c.kind), ['workload', 'workload', 'workload', 'gap']);
  assert.strictEqual(cells[0].name, 'checkout-api');
  assert.strictEqual(cells[3].name, 'report-worker · missing memory usage data');
  const html = money.html(r, state.DEFAULTS, VIEW, false, 1100);
  assert.match(html, /Savings by workload/);
  assert.match(html, /tm-gap/);
  assert.match(html, /data-workload="shop\/deployment\/checkout-api"/);
  assert.match(html, /Only 3 workloads could be counted in this file/);
  const spend = money.cells(r, money.summarize(r), Object.assign({}, VIEW, { metric: 'spend' }), false);
  assert.ok(spend[0].value >= spend[1].value);
  assert.match(spend[0].note, /waste$/);
});

test('with 1,000 workloads the map opens on namespaces and caps its workload cells', () => {
  const r = large(1000), sum = money.summarize(r);
  const ns = money.cells(r, sum, VIEW, false);
  assert.strictEqual(ns.filter(c => c.kind === 'ns').length, 20);
  assert.deepStrictEqual(ns.filter(c => c.kind === 'gap').map(c => c.name), [`${r.totals.workloads - r.totals.counted} workloads not counted`]);
  const html = money.html(r, state.DEFAULTS, VIEW, false, 1100);
  assert.match(html, /Savings by namespace/);
  assert.match(html, /Showing 8 of 20/);
  assert.match(html, /Show all 20 namespaces/);
  assert.strictEqual((html.match(/data-workload=/g) || []).length, 10);

  const wl = money.cells(r, sum, Object.assign({}, VIEW, { level: 'workloads' }), false);
  assert.strictEqual(wl.filter(c => c.kind === 'workload').length, money.MAP_CELLS - 1);
  const rest = wl.find(c => c.kind === 'rest');
  // Workloads with nothing to save have no area.
  const saving = r.workloads.filter(w => w.series && w.annualSavings > 0).length;
  assert.strictEqual(rest.name, `${saving - (money.MAP_CELLS - 1)} more workloads`);
  let total = 0;
  wl.forEach(c => { if (c.kind !== 'gap') total += c.value; });
  assert.ok(close(total, r.totals.savings));

  const one = money.cells(r, sum, Object.assign({}, VIEW, { ns: 'ns-3' }), false);
  assert.ok(one.filter(c => c.kind === 'workload').every(c => c.ns === 'ns-3'));
  assert.match(money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { ns: 'ns-3' }), false, 1100), /Savings in ns-3/);
});

test('the section for 1,000 workloads builds well inside the 200 ms budget for a lever change', () => {
  const r = large(1000);
  const views = [VIEW, Object.assign({}, VIEW, { level: 'workloads' }), Object.assign({}, VIEW, { metric: 'spend', allNs: true })];
  views.forEach(v => money.html(r, state.DEFAULTS, v, false, 1100));
  const t0 = process.hrtime.bigint();
  const runs = 20;
  for (let i = 0; i < runs; i++) money.html(r, state.DEFAULTS, views[i % views.length], false, 1100);
  const ms = Number(process.hrtime.bigint() - t0) / 1e6 / runs;
  assert.ok(ms < budget(20), `${ms.toFixed(2)} ms per build`);
});

test('provisional figures are rounded and marked', () => {
  const r = model.compute(fixture());
  const html = money.html(r, state.DEFAULTS, VIEW, true, 1100);
  assert.match(html, /~\$/);
});

test('names from the file are escaped', () => {
  const r = large(3);
  r.workloads[0].name = '<img src=x>';
  r.workloads[0].ns = 'a"b';
  const html = money.html(r, state.DEFAULTS, VIEW, false, 1100);
  assert.doesNotMatch(html, /<img/);
  assert.doesNotMatch(html, /data-workload="[^"]*a"b/);
});

test('a view with nothing to save says so, without a map of only hatched cells', () => {
  const r = large(3);
  r.workloads.forEach(w => { w.annualSavings = 0; w.savingsPct = 0; });
  r.workloads[2].series = null;
  r.totals = { spend: r.workloads[0].annualSpend + r.workloads[1].annualSpend, savings: 0, workloads: 3, counted: 2 };
  const sum = money.summarize(r);
  assert.deepStrictEqual(money.cells(r, sum, VIEW, false), []);
  const html = money.html(r, state.DEFAULTS, VIEW, false, 1100);
  assert.match(html, /No workload in this view has anything to save, so there is nothing to map\. 1 workload could not be counted\./);
  assert.doesNotMatch(html, /tm-gap/);
  assert.doesNotMatch(html, /No workload has complete data/);

  r.workloads.forEach(w => { w.annualSpend = 0; });
  r.totals.spend = 0;
  const spend = money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { metric: 'spend' }), false, 1100);
  assert.match(spend, /no spend to map/);
  assert.match(spend, /nothing requested/);
  assert.doesNotMatch(spend, /0% of/);

  const none = large(2);
  none.workloads.forEach(w => { w.series = null; });
  none.totals = { spend: 0, savings: 0, workloads: 2, counted: 0 };
  assert.match(money.html(none, state.DEFAULTS, VIEW, false, 1100), /No workload has complete data/);
});

test('the legend lists only the namespaces on the map', () => {
  const r = model.compute(fixture());
  const html = money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { ns: 'shop' }), false, 1100);
  const legend = html.match(/<div class="legend">(.*?)<\/div>/)[1];
  assert.match(legend, /shop/);
  assert.doesNotMatch(legend, /data|search/);
});

test('the shared cell for smaller workloads gives its share and how to see them', () => {
  const r = large(1000);
  const html = money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { level: 'workloads' }), false, 1100);
  assert.match(html, /more workloads<\/b><span>\$[\d,.M]+ · \d+% of savings/);
  assert.match(html, /select one to see all of its workloads/);
});

test('a drilled namespace without a colour of its own names its key', () => {
  const r = large(1000), sum = money.summarize(r);
  const ns = sum.namespaces.find(n => n.tone === 'other').name;
  const legend = money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { ns }), false, 1100).match(/<div class="legend">(.*?)<\/div>/)[1];
  assert.match(legend, new RegExp(`tm-other"></i>${ns}<`));
  assert.doesNotMatch(legend, /smaller workloads|other namespaces/);
  const all = money.html(r, state.DEFAULTS, Object.assign({}, VIEW, { level: 'workloads' }), false, 1100).match(/<div class="legend">(.*?)<\/div>/)[1];
  assert.match(all, /other namespaces/);
});
