'use strict';
const test = require('node:test');
const assert = require('node:assert');
const state = require('../src/js/state.js');
const ui = require('../src/js/components.js');

test('the defaults write an empty hash and read back as the defaults', () => {
  assert.strictEqual(state.serialize(state.DEFAULTS), '');
  assert.deepStrictEqual(state.parse(''), state.DEFAULTS);
  assert.deepStrictEqual(state.parse('#'), state.DEFAULTS);
});

test('every lever survives a round trip through the hash', () => {
  const st = Object.assign({}, state.DEFAULTS, {
    section: 'money', rates: { perCore: 0.05, perGiB: 0.0071 }, headroom: 0.25, peakMinutes: 60,
    namespaces: ['shop', 'data'], range: [1789516800, 1789603200], engineerCost: 150000, workload: 'shop/deployment/checkout-api'
  });
  const hash = state.serialize(st);
  assert.strictEqual(hash, 'section=money&cpu=0.05&gib=0.0071&headroom=25&peak=60&ns=shop,data&from=1789516800&to=1789603200&cost=150000&workload=shop/deployment/checkout-api');
  assert.deepStrictEqual(state.parse('#' + hash), st);
});

test('values that do not parse fall back to their defaults', () => {
  const st = state.parse('#section=nowhere&cpu=0.05&headroom=-5&peak=7.5&from=20&to=10&cost=abc&extra=1');
  assert.deepStrictEqual(st, state.DEFAULTS);
  assert.strictEqual(state.parse('#headroom=0').headroom, 0);
});

test('levers are passed to the model in its units', () => {
  const lv = state.levers(state.parse('#headroom=20&peak=30&ns=a'));
  assert.ok(Math.abs(lv.headroom - 1.2) < 1e-12);
  assert.strictEqual(lv.peakWindow, 1800);
  assert.deepStrictEqual(lv.namespaces, ['a']);
  assert.strictEqual(lv.rates, null);
  assert.strictEqual(lv.range, null);
});

test('the store reports which keys changed and ignores no-op sets', () => {
  const store = state.store();
  const seen = [];
  store.subscribe((st, changed) => seen.push(changed));
  store.set({ section: 'summary' });
  store.set({ headroom: 0.2, section: 'money' });
  store.set({ namespaces: ['a'] });
  store.set({ namespaces: ['a'] });
  assert.deepStrictEqual(seen, [['headroom', 'section'], ['namespaces']]);
  assert.strictEqual(state.affectsFigures(['section', 'workload']), false);
  assert.strictEqual(state.affectsFigures(['section', 'range']), true);
});

test('provisional figures are rounded to two significant figures and marked', () => {
  assert.strictEqual(ui.money(2181.4), '$2,181');
  assert.strictEqual(ui.money(2381.4, true), '~$2,400');
  assert.strictEqual(ui.money(181.6, true), '~$180');
  assert.strictEqual(ui.money(1234567), '$1.23M');
  assert.strictEqual(ui.money(NaN), '–');
  assert.strictEqual(ui.amount(5.7712), '5.77');
  assert.strictEqual(ui.amount(10.63), '10.6');
});

test('confidence follows the fidelity tier, and snapshots are provisional', () => {
  assert.strictEqual(ui.confidenceLevel({ tier: 1, snapshot: false }), 'high');
  assert.strictEqual(ui.confidenceLevel({ tier: 2, snapshot: false }), 'good');
  assert.strictEqual(ui.confidenceLevel({ tier: 3, snapshot: false }), 'provisional');
  assert.strictEqual(ui.confidenceLevel({ tier: 1, snapshot: true }), 'provisional');
  assert.match(ui.confidencePill('good'), /class="pill pill-info">Good confidence</);
});

test('components escape their text and carry their variant', () => {
  const tile = ui.kpiTile({ label: 'A <b>', value: '$1', context: 'x', variant: 'hero provisional' });
  assert.match(tile, /^<div class="kpi kpi-hero kpi-provisional">/);
  assert.match(tile, /A &lt;b&gt;/);
  assert.match(ui.kpiTile({ label: 'a', value: '1' }), /^<div class="kpi">/);
  const b = ui.banner({ tone: 'danger', title: 'No <data>', body: '<em>x</em>' });
  assert.match(b, /class="banner banner-danger" role="alert"/);
  assert.match(b, /No &lt;data&gt;/);
  assert.match(b, /<em>x<\/em>/);
  assert.match(ui.banner({ tone: 'info', body: '' }), /role="status"/);
});

test('a sample id round-trips and leads the link', () => {
  const st = Object.assign({}, state.DEFAULTS, { sample: 'demo', section: 'workloads' });
  assert.strictEqual(state.serialize(st), 'sample=demo&section=workloads');
  assert.deepStrictEqual(state.parse('#sample=demo&section=workloads'), st);
  assert.strictEqual(state.parse('#sample=../x').sample, null);
  assert.strictEqual(state.affectsFigures(['sample']), false);
});
