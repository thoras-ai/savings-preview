/* Page one: the savings argument. The headline saving, today next to
 * Thoras, the gap over the window, three workloads before and after, the
 * fleet multiplier and the next step. Plain script: registers the section in
 * the browser and exports its figures and markup under node. */
(function () {
  'use strict';

  var node = typeof module !== 'undefined' && module.exports;
  var model = node ? require('./model.js') : SP.model;
  var ui = node ? require('./components.js') : SP.ui;
  var state = node ? require('./state.js') : SP.state;
  var chart = node ? require('./chart.js') : SP.chart;
  var esc = ui.esc;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var WORKDAYS = 220;
  var DAY = 86400;
  var BOOK_DEMO = 'https://www.thoras.ai/book-a-demo';
  var LONG_RUN = 'savings-preview run --duration 168h';
  // Readings that cover less than this get the run-it-for-a-week next step.
  var FULL_WEEK = 7 * DAY;
  // A workload above half its highest usage for less than this share of the
  // window runs in bursts, like a batch job.
  var SHORT_ACTIVE = 0.2;
  // Saves at most this share of its spend: already right-sized.
  var RIGHT_SIZED = 0.05;
  var SOURCES = { prometheus: 'Prometheus', gke: 'Google Cloud Managed Prometheus', datadog: 'Datadog' };

  function day(sec) {
    var d = new Date(sec * 1000);
    return MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate();
  }

  function year(sec) { return new Date(sec * 1000).getUTCFullYear(); }

  // The dates the figures cover: the time range when one is set, else the file.
  function coverage(r, st) {
    var a = st.range ? st.range[0] : r.fidelity.start, b = st.range ? st.range[1] : r.fidelity.end;
    if (a === null || b === null) return '';
    if (day(a) === day(b) && year(a) === year(b)) return day(b) + ', ' + year(b) + ' (UTC)';
    return day(a) + (year(a) !== year(b) ? ', ' + year(a) : '') + ' – ' + day(b) + ', ' + year(b) + ' (UTC)';
  }

  // Seconds of data the figures cover.
  function windowSeconds(r, st) {
    var f = r.fidelity;
    if (f.start === null) return 0;
    if (!st.range) return f.spanSeconds;
    return Math.max(0, Math.min(st.range[1], f.end) - Math.max(st.range[0], f.start));
  }

  // "14 days" reads as "14-day" before a noun.
  function spanAdjective(seconds) {
    var parts = model.formatSpan(seconds).split(' ');
    return parts[0] + '-' + parts[1].replace(/s$/, '');
  }

  function polled(f) { return f.source.split(', ').indexOf('live polling') >= 0; }

  // Under a week of data, or a single reading, the figure misses part of a
  // full week of demand.
  function thin(r, st) { return r.fidelity.snapshot || windowSeconds(r, st) < FULL_WEEK; }

  function provenance(r, st) {
    var f = r.fidelity, span = windowSeconds(r, st), from;
    // A short poll in a compact file is one rolled-up point: readings at the
    // source step, kept as one step's mean and peak.
    if (f.snapshot && f.sourceStepSeconds) from = 'From a read of your cluster under ' + model.formatSpan(f.stepSeconds) + '.';
    else if (f.snapshot) from = 'From a single reading of your cluster.';
    else if (polled(f)) from = 'From a ' + spanAdjective(span) + ' read of your cluster.';
    else {
      var names = f.source ? f.source.split(', ').map(function (s) { return SOURCES[s] || s; }) : [];
      from = 'From ' + model.formatSpan(span) + ' of your ' + (names.length ? names.join(' and ') + ' data.' : 'cluster’s metrics.');
    }
    return from + ' Computed in your browser. Nothing installed in your cluster, nothing uploaded.';
  }

  // Usage over requests, CPU and memory weighted by their price, so the
  // figure is the share of what the cluster pays for that it uses. A
  // workload's usage counts up to its request: use above it is not paid for
  // by the request, and would put the figure over 100%.
  function utilization(r, sized) {
    var p = r.pricing, used = 0, paid = 0;
    r.workloads.forEach(function (w) {
      if (!w.series) return;
      var cores = sized ? w.reqCores - w.wasteCores : w.reqCores, gib = sized ? w.reqGiB - w.wasteGiB : w.reqGiB;
      used += Math.min(w.useCores, cores) * p.perCore + Math.min(w.useGiB, gib) * p.perGiB;
      paid += cores * p.perCore + gib * p.perGiB;
    });
    return paid > 0 ? used / paid : NaN;
  }

  // Today next to Thoras. With Thoras each workload requests its target,
  // capped at today's request, so its requests are today's less the waste.
  function figures(r) {
    var t = r.totals;
    return {
      savings: t.savings, share: t.spend > 0 ? t.savings / t.spend : 0,
      today: { spend: t.spend, utilization: utilization(r, false), cores: t.reqCores, gib: t.reqGiB },
      thoras: { spend: t.spend - t.savings, utilization: utilization(r, true), cores: t.reqCores - t.cores, gib: t.reqGiB - t.gib }
    };
  }

  // Share of the window the workload spends above half its highest usage.
  function activeShare(w) {
    var u = w.series.useCost, top = 0, n = 0, busy = 0, i;
    for (i = 0; i < u.length; i++) if (isFinite(u[i]) && u[i] > top) top = u[i];
    for (i = 0; i < u.length; i++) {
      if (!isFinite(u[i])) continue;
      n++;
      if (u[i] > top / 2) busy++;
    }
    return n ? busy / n : 1;
  }

  function most(list, key) {
    return list.reduce(function (best, w) {
      return !best || w[key] > best[key] || (w[key] === best[key] && w.name < best.name) ? w : best;
    }, null);
  }

  // Three workloads that show the range of the savings: the biggest saver
  // that runs all day, a job that runs in bursts and holds its request
  // between them, and one that is already right-sized and spends at least a
  // tenth of what the first does. Missing roles are filled with the next
  // biggest savers.
  function pick(r, st) {
    var counted = r.workloads.filter(function (w) { return w.series && w.annualSpend > 0; });
    var bursts = windowSeconds(r, st) >= DAY && !r.fidelity.snapshot;
    var short = {};
    counted.forEach(function (w) { short[w.id] = bursts && activeShare(w) < SHORT_ACTIVE; });
    var chosen = [];
    function free(w) { return chosen.indexOf(w) < 0; }
    var batch = most(counted.filter(function (w) { return short[w.id] && w.savingsPct >= 0.8; }), 'annualSavings');
    var saver = most(counted.filter(function (w) { return !short[w.id] && w.annualSavings > 0; }), 'annualSavings') ||
      most(counted.filter(function (w) { return w !== batch && w.annualSavings > 0; }), 'annualSavings');
    [saver, batch].forEach(function (w) { if (w && free(w)) chosen.push(w); });
    // A right-sized workload too small to show in dollars makes no point.
    var floor = chosen.length ? chosen[0].annualSpend / 10 : 0;
    var sized = most(counted.filter(function (w) { return free(w) && w.savingsPct <= RIGHT_SIZED && w.annualSpend >= floor; }), 'annualSpend');
    var rest = counted.filter(function (w) { return free(w) && w !== sized; })
      .sort(function (a, b) { return b.annualSavings - a.annualSavings || a.name.localeCompare(b.name); });
    while (chosen.length < (sized ? 2 : 3) && rest.length) chosen.push(rest.shift());
    if (sized) chosen.push(sized);
    return chosen;
  }

  // Workloads in scope with an OOM kill in the window.
  function oomKilled(data, r, st) {
    var inScope = {};
    r.workloads.forEach(function (w) { inScope[w.id] = true; });
    return data.workloads.filter(function (p) {
      if (!inScope[p.id]) return false;
      for (var i = 0; i < p.oom.t.length; i++) {
        if (!st.range || (p.oom.t[i] >= st.range[0] && p.oom.t[i] <= st.range[1])) return true;
      }
      return false;
    }).length;
  }

  function engineers(savings, cost) {
    var people = savings / cost;
    if (people >= 1.05) return 'about ' + ui.num(people, 1) + ' loaded engineers';
    if (people >= 0.95) return 'about one loaded engineer';
    if (people >= 0.1) return 'about ' + ui.num(people, 1) + ' of a loaded engineer';
    var days = Math.round(savings / (cost / WORKDAYS));
    return 'about ' + days + ' engineer-' + ui.plural(days, 'day');
  }

  function headline(fig) {
    return 'Thoras would cut this cluster’s compute spend by ' + ui.money(fig.savings) + ' a year, ' + ui.pct(fig.share) + '.';
  }

  function method(st) {
    return 'Target is ' + ui.num(1 + st.headroom, 2) + ' × the highest usage in any ' + st.peakMinutes + '-minute window; waste is the request above it.' +
      (st.namespaces ? ' Only the ' + ui.plural(st.namespaces.length, 'namespace') + ' ' + st.namespaces.join(', ') + ' ' + (st.namespaces.length === 1 ? 'is' : 'are') + ' included.' : '');
  }

  // Plain text for pasting into chat or email.
  function summaryText(r, st) {
    var fig = figures(r);
    return [
      'Savings preview: ' + (r.cluster || 'Unnamed cluster'),
      'Thoras would cut compute spend by ' + ui.money(fig.savings) + ' a year, ' + ui.pct(fig.share) + '.',
      'Utilization: ' + ui.pct(fig.today.utilization) + ' today, ' + ui.pct(fig.thoras.utilization) + ' with Thoras.',
      provenance(r, st)
    ].join('\n');
  }

  function capacity(cores, gib) {
    return ui.num(cores, cores < 10 ? 1 : 0) + ' cores · ' + ui.num(gib, gib < 10 ? 1 : 0) + ' GiB requested';
  }

  function side(cls, title, s) {
    return '<div class="vs-side vs-' + cls + '"><div class="vs-title">' + title + '</div>' +
      '<div class="vs-spend">' + esc(ui.money(s.spend)) + ' <span>/ yr</span></div>' +
      '<div class="vs-util">' + esc(ui.pct(s.utilization)) + ' utilization</div>' +
      '<div class="vs-cap">' + esc(capacity(s.cores, s.gib)) + '</div></div>';
  }

  function versus(fig) {
    return '<div class="vs">' + side('today', 'Today', fig.today) +
      '<div class="vs-saves"><span>saves</span><strong>' + esc(ui.money(fig.savings)) + '</strong><span>' + esc(ui.pct(fig.share)) + '</span></div>' +
      side('thoras', 'With Thoras', fig.thoras) + '</div>';
  }

  function gapCard(r, st, fig) {
    var span = coverage(r, st);
    var totals = { a: ui.money(fig.today.spend), b: ui.money(fig.thoras.spend), gap: ui.money(fig.savings) };
    var legend = chart.legend('gap').map(function (item) {
      return '<span><i class="swatch swatch-' + item.kind + '"></i>' + esc(item.name) + (totals[item.kind] ? ' <b>' + esc(totals[item.kind]) + '</b>' : '') + '</span>';
    }).join('');
    return '<div class="card"><h2>Requested today against Thoras</h2><div class="sub">Annualized compute spend' + (span ? ', ' + esc(span) : '') +
      '. The hatched gap is waste.</div><div class="legend legend-gap">' + legend + '</div><div class="chart-host sum-chart"></div></div>';
  }

  function workloadLink(st, w, text) {
    var href = '#' + state.serialize(Object.assign({}, st, { section: 'workloads', workload: w.id }));
    return '<a href="' + esc(href) + '" data-workload="' + esc(w.id) + '">' + text + '</a>';
  }

  function workloadsCard(list, st, killed) {
    if (!list.length) return '';
    var rows = list.map(function (w) {
      var today = w.annualSpend / 12, saves = w.annualSavings / 12;
      return '<tr><td>' + workloadLink(st, w, '<span class="wl-name">' + esc(w.name) + '</span>') + '<div class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</div></td>' +
        '<td class="num">' + esc(ui.money(today)) + '</td><td class="num">' + esc(ui.money(today - saves)) + '</td>' +
        '<td class="num"><span class="pill pill-brand">' + esc(ui.money(saves)) + '</span></td></tr>';
    }).join('');
    var oom = killed ? '<p class="sum-note">' + killed + ' ' + ui.plural(killed, 'workload is', 'workloads are') +
      ' being OOM-killed today. Thoras can raise their memory until the kills stop.</p>' : '';
    return '<div class="card"><h2>Three workloads, before and after</h2><div class="sub">Compute spend per month</div>' +
      '<div class="table-wrap"><table class="sum-table"><thead><tr><th scope="col">Workload</th><th scope="col" class="num">Today</th>' +
      '<th scope="col" class="num">With Thoras</th><th scope="col" class="num">Saves</th></tr></thead><tbody>' + rows + '</tbody></table></div>' + oom + '</div>';
  }

  function fleetFigures(savings, clusters, cost) {
    var total = savings * clusters;
    return '<div class="meaning-figure" data-fleet-total>' + esc(ui.money(total)) + ' a year</div>' +
      '<div class="muted" data-fleet-people>' + esc(engineers(total, cost)) + ', at ' + esc(ui.money(cost)) + ' each</div>';
  }

  function fleetCard(fig, st) {
    return '<div class="card"><h2>Across your fleet</h2><div class="fleet">' +
      '<label class="fleet-input"><span>Clusters like this one</span><input type="number" min="1" max="10000" step="1" inputmode="numeric" value="' + st.clusters + '" data-clusters></label>' +
      '<div class="fleet-figures">' + fleetFigures(fig.savings, st.clusters, st.engineerCost) + '</div></div></div>';
  }

  function nextCard(r, st) {
    var longer = thin(r, st) ? '<p class="muted">Run it for a week for a full-month figure: <code>' + esc(LONG_RUN) + '</code></p>' : '';
    return '<div class="card next"><h2>Next step</h2><p>A two-week proof of value on this cluster, read-only, with the same numbers measured live.</p>' + longer +
      '<div class="next-actions"><a class="button" href="' + BOOK_DEMO + '" target="_blank" rel="noopener">Book a demo</a>' +
      '<button type="button" class="button button-quiet" data-copy-summary>Copy summary</button></div></div>';
  }

  // The product sizes requests to a usage forecast. This page has no
  // forecast, so its target is the rolling peak, as the method line says.
  var EMPTY = 'No data in the selected time range or namespaces.';

  function markup(data, r, st) {
    if (!r.totals.counted) {
      return '<div class="sum-head"><h1>' + EMPTY + '</h1><p class="sum-from">Change them under Assumptions in Details for engineers.</p></div>';
    }
    var fig = figures(r);
    return '<div class="sum-head"><h1>' + esc(headline(fig)) + '</h1>' +
      '<p class="sum-what">Thoras forecasts what each workload will use and keeps its CPU and memory requests at that forecast plus a safety buffer, raising or lowering them as demand moves.</p>' +
      '<p class="sum-from">' + esc(provenance(r, st)) + '</p></div>' +
      versus(fig) + gapCard(r, st, fig) + workloadsCard(pick(r, st), st, oomKilled(data, r, st)) +
      '<div class="pair">' + fleetCard(fig, st) + nextCard(r, st) + '</div>' +
      '<p class="basis">' + esc(method(st)) + '</p>';
  }

  function drawChart(host, r) {
    var chartHost = host.querySelector('.sum-chart');
    if (!chartHost) return;
    if (r.series) chart.render(chartHost, r.series, 'gap', { title: 'Requested today against Thoras', fromZero: true, className: 'chart-gap' });
    else chartHost.innerHTML = '<p class="empty">No workload has complete data, so there is nothing to chart.</p>';
  }

  function copy(text, button, label) {
    function done(ok) {
      button.textContent = ok ? 'Copied' : 'Copy failed';
      clearTimeout(button.timer);
      button.timer = setTimeout(function () { button.textContent = label; }, 1500);
    }
    function fallback() {
      var t = document.createElement('textarea');
      t.value = text; t.setAttribute('readonly', ''); t.style.position = 'fixed'; t.style.opacity = '0';
      document.body.appendChild(t); t.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { /* not supported */ }
      document.body.removeChild(t);
      done(ok);
    }
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(function () { done(true); }, fallback);
    else fallback();
  }

  function render(host, ctx) {
    var r = ctx.result, st = ctx.state;
    host.ctx = ctx;
    // Typing a cluster count updates the fleet figures in place, so the input keeps focus.
    if (ctx.changed && ctx.changed.length === 1 && ctx.changed[0] === 'clusters' && host.querySelector('[data-clusters]')) {
      host.querySelector('.fleet-figures').innerHTML = fleetFigures(figures(r).savings, st.clusters, st.engineerCost);
      return;
    }
    host.innerHTML = markup(ctx.data, r, st);
    drawChart(host, r);
    if (host.bound) return;
    host.bound = true;
    host.addEventListener('click', function (e) {
      var a = e.target.closest('a[data-workload]');
      if (a) {
        e.preventDefault();
        host.ctx.set({ workload: a.getAttribute('data-workload') });
        host.ctx.go('workloads');
        return;
      }
      var b = e.target.closest('[data-copy-summary]');
      if (b) copy(summaryText(host.ctx.result, host.ctx.state), b, 'Copy summary');
    });
    host.addEventListener('input', function (e) {
      if (!e.target.matches('[data-clusters]')) return;
      var n = Math.round(Number(e.target.value));
      if (isFinite(n) && n >= 1 && n <= 10000) host.ctx.set({ clusters: n });
    });
  }

  var api = {
    EMPTY: EMPTY, coverage: coverage, provenance: provenance, thin: thin, figures: figures, utilization: utilization, pick: pick, activeShare: activeShare,
    oomKilled: oomKilled, engineers: engineers, headline: headline, method: method, summaryText: summaryText, markup: markup, day: day
  };
  if (node) module.exports = api;
  else {
    SP.summary = Object.assign(api, { drawChart: drawChart });
    SP.sections.register({ id: 'summary', number: 1, title: 'Summary', render: render, resize: function (host, ctx) { drawChart(host, ctx.result); } });
  }
})();
