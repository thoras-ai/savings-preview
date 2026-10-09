'use strict';
// Single-cluster reports render the engineer sections exactly as recorded in
// tests/golden. SP_UPDATE_GOLDEN=1 rewrites the recorded files.
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

const GOLDEN = path.join(__dirname, 'golden');
const read = name => JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'fixtures', name), 'utf8'));

function sections(doc) {
  const data = model.prepare(doc);
  const st = Object.assign({}, state.DEFAULTS);
  const result = model.compute(data, state.levers(st));
  const ctx = { data, result, state: st, changed: null, approx: false };
  const view = { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0, mode: 'cpu', last: null };
  const parts = wl.listParts(wl.rows(data, result), view, ctx);
  const first = result.workloads.filter(w => w.series)[0];
  const method = { innerHTML: '' };
  registered.method.render(method, ctx);
  return {
    workloads: [parts.title, parts.sub, parts.table, parts.range].join('\n') + '\n' + wl.detailHtml(wl.detail(data, result, first.id), view, ctx),
    money: money.html(result, st, { level: null, metric: 'savings', ns: null, allNs: false }, false, 1100),
    cluster: cluster.markup(cluster.summarize(data, result, st), false),
    method: method.innerHTML
  };
}

for (const fixture of ['minikube-poll.otlp.json', 'intermittent.otlp.json']) {
  test('a single-cluster report is unchanged: ' + fixture, () => {
    const got = sections(read(fixture));
    for (const name of Object.keys(got)) {
      const file = path.join(GOLDEN, fixture.replace('.otlp.json', '') + '.' + name + '.html');
      if (process.env.SP_UPDATE_GOLDEN) {
        fs.mkdirSync(GOLDEN, { recursive: true });
        fs.writeFileSync(file, got[name] + '\n');
        continue;
      }
      assert.strictEqual(got[name] + '\n', fs.readFileSync(file, 'utf8'), name);
    }
  });
}
