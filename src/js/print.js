/* The printed summary: page one on one letter-size sheet in the light theme,
 * under the current settings. The page renders it just before printing, and
 * only print shows it. */
(function () {
  'use strict';

  // Clusters on page one: those with a workload in the figures.
  function counted(r) { return r.clusters.filter(function (c) { return c.counted; }).length; }

  var ui = SP.ui, esc = ui.esc, summary = SP.summary;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  function today(now) {
    return MONTHS[now.getMonth()] + ' ' + now.getDate() + ', ' + now.getFullYear();
  }

  function side(cls, title, s) {
    return '<div class="print-box print-side print-' + cls + '"><div class="print-label">' + esc(title) + '</div>' +
      '<div class="print-figure">' + esc(ui.money(s.spend)) + ' / yr</div>' +
      '<div>' + esc(ui.pct(s.utilization)) + ' utilization</div>' +
      '<div class="print-muted">' + esc(ui.num(s.cores, s.cores < 10 ? 1 : 0) + ' cores · ' + ui.num(s.gib, s.gib < 10 ? 1 : 0) + ' GiB requested') + '</div></div>';
  }

  function versus(fig) {
    return '<div class="print-vs">' + side('today', 'Today', fig.today) +
      '<div class="print-saves">saves<strong>' + esc(ui.money(fig.savings)) + '</strong>' + esc(ui.pct(fig.share)) + '</div>' +
      side('thoras', 'With Thoras', fig.thoras) + '</div>';
  }

  function gap(r, st, fig) {
    var span = summary.coverage(r, st);
    var totals = { a: ui.money(fig.today.spend), b: ui.money(fig.thoras.spend), gap: ui.money(fig.savings) };
    var legend = SP.chart.legend('gap').map(function (item) {
      return '<span><i class="swatch swatch-' + item.kind + '"></i>' + esc(item.name) + (totals[item.kind] ? ' <b>' + esc(totals[item.kind]) + '</b>' : '') + '</span>';
    }).join('');
    return '<div class="print-box"><h2>Requested today against Thoras</h2><div class="print-muted">Annualized compute spend' + (span ? ', ' + esc(span) : '') +
      '. The hatched gap is waste.</div><div class="legend legend-gap">' + legend + '</div><div class="print-chart"></div></div>';
  }

  function workloads(list, killed) {
    if (!list.length) return '';
    var rows = list.map(function (w) {
      var month = w.annualSpend / 12, saves = w.annualSavings / 12;
      return '<tr><td><span class="print-name">' + esc(w.name) + '</span><span class="print-sub print-mono">' + esc(w.ns + ' · ' + w.kind) + '</span></td>' +
        '<td class="num">' + esc(ui.money(month)) + '</td><td class="num">' + esc(ui.money(month - saves)) + '</td><td class="num"><span class="pill pill-brand">' + esc(ui.money(saves)) + '</span></td></tr>';
    }).join('');
    var oom = killed ? '<p class="print-muted">' + killed + ' ' + ui.plural(killed, 'workload is', 'workloads are') +
      ' being OOM-killed today. Thoras can raise their memory until the kills stop.</p>' : '';
    return '<div class="print-box"><h2>Three workloads, before and after</h2><table class="print-table"><thead><tr><th>Workload</th><th class="num">Today / month</th>' +
      '<th class="num">With Thoras / month</th><th class="num">Saves</th></tr></thead><tbody>' + rows + '</tbody></table>' + oom + '</div>';
  }

  function clusters(r) {
    var rows = r.clusters.filter(function (c) { return c.counted; }).map(function (c) {
      return '<tr><td><span class="print-name">' + esc(c.label || c.name) + '</span></td>' +
        '<td class="num">' + esc(ui.money(c.spend)) + '</td><td class="num">' + esc(ui.money(c.spend - c.savings)) + '</td><td class="num"><span class="pill pill-brand">' + esc(ui.money(c.savings)) + '</span></td></tr>';
    }).join('');
    return '<div class="print-box"><h2>Cluster by cluster</h2><table class="print-table"><thead><tr><th>Cluster</th><th class="num">Today / year</th>' +
      '<th class="num">With Thoras / year</th><th class="num">Saves</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  function fleet(fig, st, r) {
    if (r.fleet) {
      return '<div class="print-box"><h2>Across your fleet</h2><div class="print-meaning">' + esc(ui.money(fig.savings) + ' a year') + '</div>' +
        '<div class="print-muted">' + esc(counted(r) + ' clusters; ' + summary.engineers(fig.savings, st.engineerCost) + ', at ' + ui.money(st.engineerCost) + ' each') + '</div></div>';
    }
    var total = fig.savings * st.clusters;
    return '<div class="print-box"><h2>Across your fleet</h2><div class="print-meaning">' + esc(ui.money(total) + ' a year') + '</div>' +
      '<div class="print-muted">' + esc(ui.num(st.clusters, 0) + ' ' + ui.plural(st.clusters, 'cluster') + ' like this one; ' +
        summary.engineers(total, st.engineerCost) + ', at ' + ui.money(st.engineerCost) + ' each') + '</div></div>';
  }

  function next(r, st) {
    var more = summary.longer(r, st);
    return '<div class="print-box"><h2>Next step</h2><p>A two-week proof of value on ' + (r.fleet ? 'these clusters' : 'this cluster') + ', read-only, with the same numbers measured live. Book a demo at thoras.ai/book-a-demo.</p>' +
      (more ? '<p class="print-muted">' + esc(more.text) + (more.code ? '<span class="print-mono">' + esc(more.code) + '</span>' : '') + '</p>' : '') + '</div>';
  }

  // now is the print date; tests pass a fixed one.
  function html(ctx, now) {
    var r = ctx.result, st = ctx.state, fig = summary.figures(r);
    var top = '<div class="print-top"><strong>Thoras Savings Preview</strong><span>' + esc((r.label || r.cluster || 'Unnamed cluster') + ' · printed ' + today(now || new Date())) + '</span></div>';
    if (!r.totals.counted) return top + '<h1>' + esc(summary.EMPTY) + '</h1>';
    return top +
      '<div><h1>' + esc(summary.headline(fig, r)) + '</h1>' +
      '<p>Thoras forecasts what each workload will use and keeps its CPU and memory requests at that forecast plus a safety buffer, raising or lowering them as demand moves.</p>' +
      '<p class="print-muted">' + esc(summary.provenance(r, st)) + '</p></div>' +
      versus(fig) + gap(r, st, fig) + (r.fleet ? clusters(r) : workloads(summary.pick(r, st), summary.oomKilled(ctx.data, r, st))) +
      '<div class="print-pair">' + fleet(fig, st, r) + next(r, st) + '</div>' +
      '<p class="print-foot">' + esc(summary.method(st, summary.labelsOf(ctx.data))) + ' Every workload, the node inventory and the settings used are in the report link shared with this printout.</p>';
  }

  function render(host, ctx) {
    host.innerHTML = html(ctx);
    var chart = host.querySelector('.print-chart');
    if (!chart) return;
    if (ctx.result.series) SP.chart.render(chart, ctx.result.series, 'gap', { title: 'Requested today against Thoras', fromZero: true, height: 220, patternId: 'print-hatch', className: 'chart-gap' });
    else chart.innerHTML = '<p class="empty">No workload has complete data, so there is nothing to chart.</p>';
  }

  SP.print = { render: render, html: html };
})();
