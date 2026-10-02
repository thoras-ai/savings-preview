/* The printed summary: one letter-size document in the light theme with the
 * executive summary's figures under the current settings. The page renders
 * it just before printing, and only print shows it. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc, summary = SP.summary;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  function today(now) {
    return MONTHS[now.getMonth()] + ' ' + now.getDate() + ', ' + now.getFullYear();
  }

  function head(r, st, level, now) {
    var f = r.fidelity, span = summary.coverage(r, st), filtered = st.namespaces || st.range;
    return '<div class="print-top"><strong>Thoras Savings Preview</strong><span>' + esc((r.cluster || 'Unnamed cluster') + ' · ' + f.label + ' · printed ' + today(now)) + '</span></div>' +
      '<div class="print-title"><div><h1>' + esc(r.cluster || 'Unnamed cluster') + '</h1><div class="print-muted">' +
      esc(f.label + (f.source ? ' · ' + f.source : '') + (span ? ' · ' + span : '')) +
      (filtered ? ' <span class="pill pill-neutral">Filtered</span>' : '') + '</div></div>' + ui.confidencePill(level) + '</div>';
  }

  function figure(label, value, context, cls) {
    return '<div class="print-box print-kpi' + (cls ? ' ' + cls : '') + '"><div class="print-label">' + esc(label) + '</div>' +
      '<div class="print-figure">' + esc(value) + '</div><div class="print-muted">' + esc(context) + '</div></div>';
  }

  function kpis(t, approx) {
    var share = t.spend > 0 ? t.savings / t.spend : 0, edge = approx ? ' print-provisional' : '';
    return '<div class="print-kpis">' +
      figure('Annual savings opportunity', ui.money(t.savings, approx), ui.pct(share) + ' of projected spend', 'print-hero' + edge) +
      figure('Projected annual spend', ui.money(t.spend, approx), 'Priced from current requests', edge.trim()) +
      figure('CPU that could be freed', ui.num(t.cores, t.cores < 1 ? 2 : 1) + ' cores', 'of ' + ui.num(t.reqCores, t.reqCores < 10 ? 1 : 0) + ' cores requested', edge.trim()) +
      figure('Memory that could be freed', ui.num(t.gib, t.gib < 1 ? 2 : 1) + ' GiB', 'of ' + ui.num(t.reqGiB, t.reqGiB < 10 ? 1 : 0) + ' GiB requested', edge.trim()) +
      '</div>';
  }

  function confidence(r, level, st) {
    var t = r.totals, out = '<p>' + ui.confidencePill(level) + ' ' + esc(summary.confidenceText(r.fidelity, level, st.peakMinutes)) + '</p>';
    var left = r.workloads.filter(function (w) { return !w.series; });
    if (left.length) {
      var named = left.slice(0, 3).map(function (w) { return w.name + ' (' + w.ns + ') ' + w.reason.charAt(0).toLowerCase() + w.reason.slice(1); });
      out += '<p class="print-muted">' + ui.num(left.length) + ' of ' + ui.num(t.workloads) + ' ' + ui.plural(t.workloads, 'workload') + ' ' +
        (left.length === 1 ? 'has' : 'have') + ' too little data for a savings figure and ' + (left.length === 1 ? 'is' : 'are') + ' not in the totals: ' +
        esc(named.join('; ') + (left.length > 3 ? '; and ' + (left.length - 3) + ' more' : '')) + '.</p>';
    }
    r.fidelity.notes.forEach(function (n) { out += '<p class="print-muted">' + esc(n) + '</p>'; });
    return '<div class="print-box"><h2>Confidence</h2>' + out + '</div>';
  }

  function meaning(t, approx, st) {
    var share = t.spend > 0 ? t.savings / t.spend : 0;
    var fig = function (big, small) { return '<div><div class="print-meaning">' + esc(big) + '</div><div class="print-muted">' + esc(small) + '</div></div>'; };
    return '<div class="print-box"><h2>What this means</h2><div class="print-meanings">' +
      fig(ui.money(t.savings / 12, approx), 'per month, at current prices') +
      fig(summary.engineers(t.savings, st.engineerCost), 'a year, at ' + ui.money(st.engineerCost) + ' loaded cost') +
      fig(ui.pct(share), 'of this cluster’s projected spend') + '</div></div>';
  }

  function spend(r, approx) {
    var t = r.totals, s = r.series;
    var first = s && summary.day(s.t[0]), last = s && summary.day(s.t[s.t.length - 1]);
    var range = s ? (first === last ? first : first + ' – ' + last) + ' (UTC)' : '';
    var totals = { a: ui.money(t.spend, approx), b: ui.money(t.spend - t.savings, approx) };
    var legend = SP.chart.legend('spend').map(function (item) {
      return '<span><i class="swatch swatch-' + item.kind + '"></i>' + esc(item.name) + (totals[item.kind] ? ' <b>' + esc(totals[item.kind]) + '</b>' : '') + '</span>';
    }).join('');
    return '<div class="print-box"><h2>Spend over the window</h2><div class="print-muted">Annualized hourly spend' + (range ? ', ' + esc(range) : '') + '. ' +
      ui.num(t.counted) + ' of ' + ui.num(t.workloads) + ' ' + ui.plural(t.workloads, 'workload') + '.</div>' +
      '<div class="legend">' + legend + '</div><div class="print-chart"></div></div>';
  }

  function top(r, approx) {
    var list = summary.topWorkloads(r);
    if (!list.length) return '';
    var max = list[0].annualSavings;
    return '<div class="print-box"><h2>Top opportunities</h2><div class="print-ranks">' + list.map(function (w) {
      return '<div><span class="print-name">' + esc(w.name) + '</span><span class="print-sub print-mono">' + esc(w.ns + ' · ' + w.kind) + '</span></div>' +
        '<div class="print-bar"><i style="width:' + Math.max(1, Math.round(w.annualSavings / max * 100)) + '%"></i></div>' +
        '<div class="print-value">' + esc(ui.money(w.annualSavings, approx)) + '<span class="print-sub">' + ui.pct(w.savingsPct) + ' of its spend</span></div>';
    }).join('') + '</div></div>';
  }

  function method(r, st) {
    var scope = st.namespaces ? ' Only the namespaces ' + st.namespaces.join(', ') + ' are counted.' : '';
    return '<div class="print-box"><h2>How the figure is computed</h2><p>' + esc('Target is ' + ui.num(1 + st.headroom, 2) + ' times the highest usage in any ' +
      st.peakMinutes + '-minute window, per workload, for CPU and memory. Waste is the amount requested above the target. A workload’s cost is the larger of its CPU and memory cost, priced with ' +
      r.pricing.basis + ', averaged over ' + (st.range ? 'the selected time range' : 'the window') + ' and annualized at 8,760 hours. Replica counts are taken as they were; horizontal scaling is out of scope.' + scope) + '</p></div>';
  }

  // now is the print date; tests pass a fixed one.
  function html(ctx, now) {
    var r = ctx.result, st = ctx.state, approx = ctx.approx, level = ui.confidenceLevel(r.fidelity);
    return head(r, st, level, now || new Date()) +
      (level === 'provisional' ? ui.banner({ tone: 'caution', title: 'Provisional.', body: esc((r.fidelity.snapshot ? 'A single snapshot' : r.fidelity.label) + ' is too little history for a dependable figure. Numbers are rounded and marked with ~.') }) : '') +
      kpis(r.totals, approx) + confidence(r, level, st) + meaning(r.totals, approx, st) + spend(r, approx) + top(r, approx) + method(r, st) +
      '<p class="print-foot">Full report, with every workload, the node inventory and the settings used: open the report link shared with this printout. Computed locally from the cluster’s own metrics; nothing was uploaded.</p>';
  }

  function render(host, ctx) {
    host.innerHTML = html(ctx);
    var chart = host.querySelector('.print-chart');
    if (ctx.result.series) SP.chart.render(chart, ctx.result.series, 'spend', { title: 'Spend over the window', fromZero: true, height: 240, patternId: 'print-hatch' });
    else chart.innerHTML = '<p class="empty">No workload has complete data, so there is nothing to chart.</p>';
  }

  SP.print = { render: render, html: html };
})();
