/* Section 1: the executive summary. Headline figures, how far the data can be
 * trusted, the savings in budget terms, spend over the window and the
 * workloads with the most to save. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var WORKDAYS = 220;
  var TOP = 5;

  function day(sec) {
    var d = new Date(sec * 1000);
    return MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate();
  }

  function confidenceText(f, level, peakMinutes) {
    var res = f.label;
    if (f.snapshot) return 'A single snapshot. It shows the gap between requests and usage now, with no history to find peaks in. Treat it as a direction, not a figure.';
    if (level === 'high') return res + '. The ' + peakMinutes + '-minute peaks that set the target are fully captured, so this figure is as reliable as the method allows.';
    if (level === 'good') return res + '. Samples this coarse smooth peaks shorter than one step, which makes the target slightly low and the saving slightly high. Expect the real figure to be a little lower.';
    return res + '. A window this short cannot show daily or weekly peaks, and coarse samples hide short ones, so the figure can move a lot with more data. Treat it as a direction, not a budget line.';
  }

  function heading(r, st, level) {
    var filtered = st.namespaces || st.range;
    return '<div class="sum-head"><div><h1>' + esc(r.cluster || 'Unnamed cluster') + '</h1>' +
      '<div class="sum-meta">' + esc(r.fidelity.label) + (r.fidelity.source ? ' · ' + esc(r.fidelity.source) : '') +
      (filtered ? ' <span class="pill pill-neutral">Filtered</span>' : '') + '</div></div>' +
      ui.confidencePill(level) + '</div>';
  }

  function provisionalBanner(f) {
    var what = f.snapshot ? 'A single snapshot' : f.label;
    return ui.banner({
      tone: 'caution', title: 'Provisional.',
      body: esc(what) + ' is too little history for a dependable figure. Numbers are rounded and marked with ~. At least 7 days of metrics give a figure to budget on.'
    });
  }

  function tiles(t, approx) {
    var edge = approx ? ' provisional' : '';
    var share = t.spend > 0 ? t.savings / t.spend : 0;
    return '<div class="kpis">' +
      ui.kpiTile({ label: 'Annual savings opportunity', value: ui.money(t.savings, approx), context: ui.pct(share) + ' of projected spend', variant: 'hero' + edge }) +
      ui.kpiTile({ label: 'Projected annual spend', value: ui.money(t.spend, approx), context: 'Priced from current requests', variant: 'default' + edge }) +
      ui.kpiTile({ label: 'CPU that could be freed', value: ui.amount(t.cores) + ' cores', context: 'of ' + (t.reqCores < 10 ? ui.amount(t.reqCores) : ui.num(t.reqCores, 0)) + ' cores requested', variant: 'default' + edge }) +
      ui.kpiTile({ label: 'Memory that could be freed', value: ui.amount(t.gib) + ' GiB', context: 'of ' + (t.reqGiB < 10 ? ui.amount(t.reqGiB) : ui.num(t.reqGiB, 0)) + ' GiB requested', variant: 'default' + edge }) +
      '</div>';
  }

  function confidenceCard(r, level, st) {
    var t = r.totals, out = [];
    out.push('<p>' + ui.confidencePill(level) + ' ' + esc(confidenceText(r.fidelity, level, st.peakMinutes)) + '</p>');
    var left = t.workloads - t.counted;
    if (left) {
      out.push('<p class="muted"><strong>' + left + ' of ' + t.workloads + ' ' + ui.plural(t.workloads, 'workload') + '</strong> ' +
        (left === 1 ? 'has' : 'have') + ' too little data for a savings figure and ' + (left === 1 ? 'is' : 'are') +
        ' not in the totals. <a href="#section=method" data-go="method">See what is missing.</a></p>');
    }
    var partial = r.workloads.filter(function (w) { return w.status === 'partial'; }).length;
    if (partial) {
      out.push('<p class="muted"><strong>' + partial + ' ' + ui.plural(partial, 'workload') + '</strong> ' + (partial === 1 ? 'has' : 'have') +
        ' containers with missing data. Those containers are left out of the workload figures.</p>');
    }
    r.fidelity.notes.forEach(function (n) { out.push('<p class="muted">' + esc(n) + '</p>'); });
    return '<div class="card"><h2>Confidence</h2><div class="sub">What the data can and cannot support</div><div class="card-body">' + out.join('') + '</div></div>';
  }

  function engineers(savings, cost) {
    var people = savings / cost;
    if (people >= 0.5) return ui.num(people, 1) + ' ' + ui.plural(Math.round(people * 10) / 10, 'engineer');
    var days = Math.round(savings / (cost / WORKDAYS));
    return days + ' engineer-' + ui.plural(days, 'day');
  }

  function meaningCard(t, approx, st) {
    var share = t.spend > 0 ? t.savings / t.spend : 0;
    var fig = function (big, small) { return '<div class="meaning"><div class="meaning-figure">' + esc(big) + '</div><div class="muted">' + small + '</div></div>'; };
    return '<div class="card"><h2>What this means</h2><div class="sub">The savings in terms a budget owner uses</div><div class="meanings">' +
      fig(ui.money(t.savings / 12, approx), 'per month, at current prices') +
      fig(engineers(t.savings, st.engineerCost), 'a year, at ' + esc(ui.money(st.engineerCost)) + ' loaded cost') +
      fig(ui.pct(share), 'of this cluster’s projected spend') +
      '</div></div>';
  }

  function spendCard(r, approx) {
    var t = r.totals, s = r.series;
    var range = s ? day(s.t[0]) + ' – ' + day(s.t[s.t.length - 1]) + ' (UTC)' : '';
    var totals = { a: ui.money(t.spend, approx), b: ui.money(t.spend - t.savings, approx) };
    var legend = SP.chart.legend('spend').map(function (item) {
      return '<span><i class="swatch swatch-' + item.kind + '"></i>' + esc(item.name) + (totals[item.kind] ? ' <b>' + esc(totals[item.kind]) + '</b>' : '') + '</span>';
    }).join('');
    return '<div class="card"><h2>Spend over the window</h2><div class="sub">Annualized hourly spend' + (range ? ', ' + esc(range) : '') + '. ' +
      t.counted + ' of ' + t.workloads + ' ' + ui.plural(t.workloads, 'workload') + '.</div>' +
      '<div class="legend">' + legend + '</div><div class="chart-host sum-chart"></div></div>';
  }

  function topCard(r, approx) {
    var top = r.workloads.filter(function (w) { return w.series && w.annualSavings > 0; })
      .sort(function (a, b) { return b.annualSavings - a.annualSavings || a.name.localeCompare(b.name); }).slice(0, TOP);
    if (!top.length) return '';
    var max = top[0].annualSavings;
    var rows = top.map(function (w) {
      return '<li><button type="button" class="rank" data-workload="' + esc(w.id) + '" aria-label="Show ' + esc(w.name) + ' in Workloads">' +
        '<span class="rank-name"><span>' + esc(w.name) + '</span><span class="rank-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</span></span>' +
        '<span class="rank-bar"><i style="width:' + Math.max(1, Math.round(w.annualSavings / max * 100)) + '%"></i></span>' +
        '<span class="rank-value"><span>' + esc(ui.money(w.annualSavings, approx)) + '</span><span class="rank-sub">' + ui.pct(w.savingsPct) + ' of its spend</span></span>' +
        '</button></li>';
    }).join('');
    return '<div class="card"><h2>Top opportunities</h2><div class="sub">Where most of the savings are</div><ol class="ranks">' + rows + '</ol></div>';
  }

  function basis(r, st) {
    return 'Priced with ' + r.pricing.basis + '. Target is ' + ui.num(1 + st.headroom, 2) + ' times the highest usage in any ' + st.peakMinutes +
      '-minute window. Waste is the amount requested above the target.';
  }

  function render(host, ctx) {
    var r = ctx.result, st = ctx.state, approx = ctx.approx;
    var level = ui.confidenceLevel(r.fidelity);
    host.classList.toggle('provisional', approx);
    host.innerHTML = heading(r, st, level) + (level === 'provisional' ? provisionalBanner(r.fidelity) : '') +
      tiles(r.totals, approx) +
      '<div class="pair">' + confidenceCard(r, level, st) + meaningCard(r.totals, approx, st) + '</div>' +
      spendCard(r, approx) + topCard(r, approx) +
      '<p class="basis">' + esc(basis(r, st)) + '</p>';
    var chartHost = host.querySelector('.sum-chart');
    if (r.series) SP.chart.render(chartHost, r.series, 'spend', { title: 'Spend over the window', fromZero: true });
    else chartHost.innerHTML = '<p class="empty">No workload has complete data, so there is nothing to chart.</p>';
    host.querySelector('.ranks') && host.querySelector('.ranks').addEventListener('click', function (e) {
      var b = e.target.closest('button[data-workload]');
      if (b) { ctx.set({ workload: b.getAttribute('data-workload') }); ctx.go('workloads'); }
    });
  }

  SP.sections.register({ id: 'summary', number: 1, title: 'Summary', render: render, resize: function (host, ctx) {
    var chartHost = host.querySelector('.sum-chart');
    if (chartHost && ctx.result.series) SP.chart.render(chartHost, ctx.result.series, 'spend', { title: 'Spend over the window', fromZero: true });
  } });
})();
