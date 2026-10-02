/* The page's shared state: which section is in view and the levers every
 * section reads. Both are kept in the URL hash so a configured view can be
 * shared. Plain script: runs in the browser (SP.state) and under node.
 *
 * Hash keys, written only when they differ from the default:
 *   section   summary, money, workloads, cluster, assumptions or method
 *   cpu, gib  USD per core-hour and per GiB-hour; both or neither (prices from the nodes)
 *   node      the instance type the rates came from, as cloud/region/type (none)
 *   discount  spot or committed-use discount on the rates, in percent (0)
 *   headroom  target headroom over the peak, in percent (10)
 *   peak      rolling peak window, in minutes (15)
 *   ns        namespaces to count, comma separated (all)
 *   from, to  time range, Unix seconds (the whole file)
 *   cost      loaded cost of an engineer per year, in USD (200000)
 *   bill      annual cloud bill, in USD (none)
 *   clusters  clusters like this one, for the fleet figure (1)
 *   workload  the workload in focus, as namespace/kind/name (none)
 */
(function () {
  'use strict';

  var SECTIONS = ['summary', 'money', 'workloads', 'cluster', 'assumptions', 'method'];

  var DEFAULTS = {
    section: 'summary',
    rates: null,
    node: null,
    discount: 0,
    headroom: 0.1,
    peakMinutes: 15,
    namespaces: null,
    range: null,
    engineerCost: 200000,
    bill: null,
    clusters: 1,
    workload: null
  };

  function positive(v) {
    var n = Number(v);
    return v !== null && v !== '' && isFinite(n) && n > 0 ? n : null;
  }

  // Unknown keys and values that do not parse fall back to the default, so a
  // hand-edited or older link still opens.
  function parse(hash) {
    var q = new URLSearchParams(String(hash || '').replace(/^#/, ''));
    var st = Object.assign({}, DEFAULTS);
    if (SECTIONS.indexOf(q.get('section')) >= 0) st.section = q.get('section');
    var cpu = positive(q.get('cpu')), gib = positive(q.get('gib'));
    if (cpu && gib) st.rates = { perCore: cpu, perGiB: gib };
    if (st.rates && /^[^/]+\/[^/]+\/[^/]+$/.test(q.get('node') || '')) st.node = q.get('node');
    var d = Number(q.get('discount'));
    if (q.get('discount') && isFinite(d) && d >= 0 && d < 100) st.discount = d / 100;
    var h = Number(q.get('headroom'));
    if (q.has('headroom') && isFinite(h) && h >= 0 && h <= 200) st.headroom = h / 100;
    var w = positive(q.get('peak'));
    if (w && w <= 1440 && w === Math.round(w)) st.peakMinutes = w;
    if (q.get('ns')) st.namespaces = q.get('ns').split(',').filter(Boolean);
    var from = Number(q.get('from')), to = Number(q.get('to'));
    if (q.get('from') && q.get('to') && isFinite(from) && isFinite(to) && from < to) st.range = [from, to];
    var cost = positive(q.get('cost'));
    if (cost) st.engineerCost = cost;
    var bill = positive(q.get('bill'));
    if (bill) st.bill = bill;
    var clusters = positive(q.get('clusters'));
    if (clusters && clusters <= 10000 && clusters === Math.round(clusters)) st.clusters = clusters;
    if (q.get('workload')) st.workload = q.get('workload');
    return st;
  }

  function round(v) { return String(Number(v.toPrecision(6))); }

  function serialize(st) {
    var q = new URLSearchParams();
    if (st.section && st.section !== DEFAULTS.section) q.set('section', st.section);
    if (st.rates) { q.set('cpu', round(st.rates.perCore)); q.set('gib', round(st.rates.perGiB)); }
    if (st.rates && st.node) q.set('node', st.node);
    if (st.discount) q.set('discount', round(st.discount * 100));
    if (Math.abs(st.headroom - DEFAULTS.headroom) > 1e-9) q.set('headroom', round(st.headroom * 100));
    if (st.peakMinutes !== DEFAULTS.peakMinutes) q.set('peak', String(st.peakMinutes));
    if (st.namespaces) q.set('ns', st.namespaces.join(','));
    if (st.range) { q.set('from', String(st.range[0])); q.set('to', String(st.range[1])); }
    if (st.engineerCost !== DEFAULTS.engineerCost) q.set('cost', round(st.engineerCost));
    if (st.bill) q.set('bill', round(st.bill));
    if (st.clusters !== DEFAULTS.clusters) q.set('clusters', String(st.clusters));
    if (st.workload) q.set('workload', st.workload);
    // Commas and slashes stay readable.
    return q.toString().replace(/%2C/g, ',').replace(/%2F/g, '/');
  }

  // The levers in the form model.compute takes.
  function levers(st) {
    return {
      rates: st.rates, discount: st.discount, headroom: 1 + st.headroom, peakWindow: st.peakMinutes * 60,
      namespaces: st.namespaces, range: st.range
    };
  }

  // A store that tells its listeners which keys changed.
  function store(initial) {
    var st = Object.assign({}, DEFAULTS, initial), subs = [];
    return {
      get: function () { return st; },
      set: function (patch) {
        var changed = Object.keys(patch).filter(function (k) { return JSON.stringify(patch[k]) !== JSON.stringify(st[k]); });
        if (!changed.length) return;
        st = Object.assign({}, st, patch);
        subs.forEach(function (fn) { fn(st, changed); });
      },
      subscribe: function (fn) { subs.push(fn); }
    };
  }

  // Keys that change the figures, as opposed to what the page shows.
  var VIEW_KEYS = ['section', 'engineerCost', 'bill', 'clusters', 'node', 'workload'];
  function affectsFigures(changed) {
    return changed.some(function (k) { return VIEW_KEYS.indexOf(k) < 0; });
  }

  var api = { SECTIONS: SECTIONS, DEFAULTS: DEFAULTS, parse: parse, serialize: serialize, levers: levers, store: store, affectsFigures: affectsFigures };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).state = api;
})();
