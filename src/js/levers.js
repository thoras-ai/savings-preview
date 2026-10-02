/* What the assumptions section offers for each lever: the peak windows the
 * data can resolve, the time range choices, which levers differ from their
 * defaults and what resets them, and the figures shown with the result.
 * Plain script: runs in the browser (SP.levers) and under node.
 */
(function () {
  'use strict';

  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var DAY = 86400, HOUR = 3600;

  // Minutes, in slider order.
  var PEAK_WINDOWS = [5, 10, 15, 30, 60, 120];

  // A window shorter than the data's step cannot be resolved: the rolling
  // peak over it is the single point it holds.
  function peakWindows(stepSeconds, snapshot) {
    return PEAK_WINDOWS.map(function (m) {
      return { minutes: m, disabled: !!snapshot || m * 60 < stepSeconds };
    });
  }

  // The slider position for a window, which may come from a link and not be
  // one of the choices: the nearest choice.
  function peakIndex(minutes) {
    var best = 0;
    PEAK_WINDOWS.forEach(function (m, i) {
      if (Math.abs(Math.log(m / minutes)) < Math.abs(Math.log(PEAK_WINDOWS[best] / minutes))) best = i;
    });
    return best;
  }

  function two(n) { return (n < 10 ? '0' : '') + n; }

  function when(sec, unit) {
    var d = new Date(sec * 1000), day = MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate();
    if (unit === 'day' && d.getUTCHours() === 0 && d.getUTCMinutes() === 0) return day;
    return day + ', ' + two(d.getUTCHours()) + ':' + two(d.getUTCMinutes());
  }

  // Choices for the start and end of the time range: whole UTC days for two
  // days of data or more, whole hours for two hours or more, else none. The
  // first start and the last end are the edges of the data. A range from a
  // link that falls between choices is added as its own choice.
  function rangeChoices(fidelity, range) {
    var a = fidelity.start, b = fidelity.end;
    if (a === null || b === null || fidelity.snapshot) return null;
    var span = b - a, unit = span >= 2 * DAY ? 'day' : span >= 2 * HOUR ? 'hour' : null;
    if (!unit) return null;
    var size = unit === 'day' ? DAY : HOUR, marks = [];
    for (var m = Math.floor(a / size) * size + size; m < b; m += size) marks.push(m);
    var from = [{ value: a, label: 'Start of data' }], to = [];
    marks.forEach(function (m) { from.push({ value: m, label: when(m, unit) }); to.push({ value: m, label: when(m, unit) }); });
    to.push({ value: b, label: 'End of data' });
    if (range) {
      add(from, range[0]);
      add(to, range[1]);
    }
    return { unit: unit, from: from, to: to, start: a, end: b };
  }

  function add(list, v) {
    if (list.some(function (o) { return o.value === v; })) return;
    list.push({ value: v, label: when(v, 'exact') });
    list.sort(function (x, y) { return x.value - y.value; });
  }

  // The range a pair of choices stands for: null when they are the edges.
  function rangeFor(choices, from, to) {
    if (from <= choices.start && to >= choices.end) return null;
    return [from, to];
  }

  // The namespaces to count after one is switched on or off. All of them is
  // null; the last one cannot be switched off.
  function toggleNamespace(current, all, ns, on) {
    var set = current ? current.slice() : all.slice(), i = set.indexOf(ns);
    if (on && i < 0) set.push(ns);
    if (!on && i >= 0) {
      if (set.length === 1) return current;
      set.splice(i, 1);
    }
    if (all.every(function (n) { return set.indexOf(n) >= 0; })) return null;
    return all.filter(function (n) { return set.indexOf(n) >= 0; });
  }

  // Each lever as the state keys it owns. Reset sets them to their defaults.
  var LEVERS = {
    headroom: ['headroom'],
    peak: ['peakMinutes'],
    rates: ['rates', 'node'],
    discount: ['discount'],
    bill: ['bill'],
    cost: ['engineerCost'],
    namespaces: ['namespaces'],
    range: ['range']
  };

  function same(a, b) { return JSON.stringify(a) === JSON.stringify(b); }

  // The levers that differ from their defaults, by name.
  function changed(st, defaults) {
    var out = {};
    Object.keys(LEVERS).forEach(function (l) {
      out[l] = LEVERS[l].some(function (k) { return !same(st[k], defaults[k]); });
    });
    return out;
  }

  function anyChanged(st, defaults) {
    var c = changed(st, defaults);
    return Object.keys(c).some(function (l) { return c[l]; });
  }

  function resetPatch(names, defaults) {
    var patch = {};
    names.forEach(function (l) { LEVERS[l].forEach(function (k) { patch[k] = defaults[k]; }); });
    return patch;
  }

  // The result strip: label, value and an optional note, with separators
  // before the capacity and the counted workloads, as in the summary.
  function figures(r, st, ui, approx) {
    var t = r.totals, share = t.spend > 0 ? t.savings / t.spend : NaN;
    var amount = function (v, unit) {
      var x = approx ? ui.rough(v) : v;
      return (approx ? '~' : '') + ui.num(x, x < 1 ? 2 : 1) + ' ' + unit;
    };
    var out = [
      { label: 'Annual savings', value: ui.money(t.savings, approx) },
      { label: 'Projected spend', value: ui.money(t.spend, approx) },
      { label: 'Share of spend', value: ui.pct(share), note: st.bill ? pctOf(t.savings, st.bill) + ' of the cloud bill' : '' },
      { label: 'CPU to free', value: amount(t.cores, 'cores'), sep: true },
      { label: 'Memory to free', value: amount(t.gib, 'GiB') },
      { label: 'Workloads counted', value: t.counted + ' of ' + t.workloads, sep: true }
    ];
    return out;
  }

  // Small shares keep one decimal, so a saving is not shown as 0%.
  function pctOf(part, whole) {
    var p = part / whole * 100;
    if (!isFinite(p)) return '–';
    return (p < 10 ? Number(p.toFixed(1)) : Math.round(p)) + '%';
  }

  var api = {
    PEAK_WINDOWS: PEAK_WINDOWS, LEVERS: LEVERS, peakWindows: peakWindows, peakIndex: peakIndex, rangeChoices: rangeChoices, rangeFor: rangeFor,
    toggleNamespace: toggleNamespace, changed: changed, anyChanged: anyChanged, resetPatch: resetPatch, figures: figures, pctOf: pctOf, when: when
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).levers = api;
})();
