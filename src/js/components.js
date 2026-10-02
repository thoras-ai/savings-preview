/* Shared pieces every section uses: number formats, the KPI tile, the
 * confidence pill and the banner. Each returns an HTML string. */
(function () {
  'use strict';

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; });
  }

  function plural(n, one, many) { return n === 1 ? one : (many || one + 's'); }

  // Two significant figures, for figures the data cannot support more precisely.
  function rough(v) {
    if (v === 0 || !isFinite(v)) return v;
    var p = Math.pow(10, Math.floor(Math.log10(Math.abs(v))) - 1);
    return Math.round(v / p) * p;
  }

  // approx rounds the figure and marks it with ~.
  function money(v, approx) {
    if (!isFinite(v)) return '–';
    var x = approx ? rough(v) : v;
    var s = Math.abs(x) >= 1e6 ? '$' + (x / 1e6).toFixed(2) + 'M' : '$' + Math.round(x).toLocaleString('en-US');
    return (approx ? '~' : '') + s;
  }

  function num(v, digits) {
    if (!isFinite(v)) return '–';
    return v.toLocaleString('en-US', { maximumFractionDigits: digits === undefined ? 1 : digits });
  }

  function pct(v) { return isFinite(v) ? Math.round(v * 100) + '%' : '–'; }

  // Three figures for amounts under 10, so 5.77 cores and 10.6 GiB.
  function amount(v) { return num(v, Math.abs(v) < 10 ? 2 : 1); }

  // How far the data can be trusted: snapshots and limited history are provisional.
  var CONFIDENCE = {
    high: { label: 'High confidence', tone: 'brand' },
    good: { label: 'Good confidence', tone: 'info' },
    provisional: { label: 'Provisional', tone: 'caution' }
  };

  function confidenceLevel(fidelity) {
    if (fidelity.snapshot || fidelity.tier >= 3) return 'provisional';
    return fidelity.tier === 1 ? 'high' : 'good';
  }

  function confidencePill(level) {
    var c = CONFIDENCE[level];
    return '<span class="pill pill-' + c.tone + '">' + esc(c.label) + '</span>';
  }

  // variant: 'default', 'hero' (the headline figure, in brand colour) or
  // 'provisional' (dashed caution edge). hero and provisional combine.
  function kpiTile(opts) {
    var cls = ['kpi'];
    (opts.variant || 'default').split(' ').forEach(function (v) { if (v !== 'default') cls.push('kpi-' + v); });
    return '<div class="' + cls.join(' ') + '"><div class="kpi-label">' + esc(opts.label) + '</div>' +
      '<div class="kpi-value">' + esc(opts.value) + '</div>' +
      (opts.context ? '<div class="kpi-context">' + esc(opts.context) + '</div>' : '') + '</div>';
  }

  // tone: 'caution', 'info' or 'danger'. body is HTML; title is text.
  function banner(opts) {
    var role = opts.tone === 'danger' ? 'alert' : 'status';
    return '<div class="banner banner-' + opts.tone + '" role="' + role + '">' +
      (opts.title ? '<strong class="banner-title">' + esc(opts.title) + '</strong>' : '') +
      '<div class="banner-body">' + opts.body + '</div></div>';
  }

  var api = {
    esc: esc, plural: plural, rough: rough, money: money, num: num, pct: pct, amount: amount,
    confidenceLevel: confidenceLevel, confidencePill: confidencePill, confidence: CONFIDENCE,
    kpiTile: kpiTile, banner: banner
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).ui = api;
})();
