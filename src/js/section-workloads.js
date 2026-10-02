/* Section 3: workloads. The requested-versus-target chart for the cluster or
 * one workload, and the workload table. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc, chart = SP.chart, model = SP.model;
  var view = { mode: 'spend', sort: { key: 'annualSavings', dir: -1 } };

  var COLUMNS = [
    { key: 'name', label: 'Workload', num: false },
    { key: 'annualSpend', label: 'Annual spend', num: true },
    { key: 'annualSavings', label: 'Annual savings', num: true },
    { key: 'savingsPct', label: 'Waste', num: true },
    { key: 'wasteCores', label: 'CPU to free', num: true },
    { key: 'wasteGiB', label: 'Memory to free', num: true }
  ];

  function selected(ctx) {
    var id = ctx.state.workload;
    return ctx.result.workloads.filter(function (w) { return w.id === id && w.series; })[0] || null;
  }

  function cmp(a, b, key, dir) {
    var av = a[key], bv = b[key];
    if (typeof av === 'string') return dir * av.localeCompare(bv);
    return dir * (av - bv);
  }

  function workloadNotes(w) {
    return w.notes.map(function (n) { return '<div class="wl-note">' + esc(model.notes[n].workload) + '</div>'; }).join('');
  }

  function renderChart(host, ctx) {
    var w = selected(ctx), r = ctx.result;
    var series = w ? w.series : r.series;
    host.querySelector('.wl-chart-title').textContent = w ? w.name : 'Whole cluster';
    host.querySelector('.wl-chart-sub').textContent = w ? w.ns + ' · ' + w.kind : r.totals.counted + ' ' + ui.plural(r.totals.counted, 'workload');
    host.querySelector('.wl-back').hidden = !w;
    host.querySelector('.tabs').innerHTML = Object.keys(chart.modes).map(function (k) {
      return '<button type="button" role="tab" aria-selected="' + (k === view.mode) + '" data-mode="' + k + '">' + chart.modes[k].label + '</button>';
    }).join('');
    host.querySelector('.legend').innerHTML = chart.legend(view.mode).map(function (item) {
      return '<span><i class="swatch swatch-' + item.kind + '"></i>' + esc(item.name) + '</span>';
    }).join('');
    var el = host.querySelector('.wl-chart');
    if (!series) { el.innerHTML = '<p class="empty">No workload has enough data to chart.</p>'; return; }
    chart.render(el, series, view.mode, { title: w ? w.name : 'Whole cluster' });
  }

  function renderTable(host, ctx) {
    var s = view.sort, approx = ctx.approx, focus = ctx.state.workload;
    var rows = ctx.result.workloads;
    var ok = rows.filter(function (w) { return w.series; }).sort(function (a, b) { return cmp(a, b, s.key, s.dir) || a.name.localeCompare(b.name); });
    var rest = rows.filter(function (w) { return !w.series; }).sort(function (a, b) { return a.name.localeCompare(b.name); });
    var head = COLUMNS.map(function (c) {
      var active = s.key === c.key;
      return '<th scope="col" class="' + (c.num ? 'num' : '') + '" aria-sort="' + (active ? (s.dir < 0 ? 'descending' : 'ascending') : 'none') + '">' +
        '<button type="button" data-sort="' + c.key + '">' + c.label + (active ? (s.dir < 0 ? ' ↓' : ' ↑') : '') + '</button></th>';
    }).join('');
    var maxPct = Math.max.apply(null, [0.0001].concat(ok.map(function (w) { return w.savingsPct; })));
    var body = ok.map(function (w) {
      var label = esc(w.name) + (w.status === 'partial' ? ' <span class="pill pill-neutral" title="' + esc(w.reason) + '">Partial data</span>' : '');
      return '<tr class="' + (w.id === focus ? 'selected' : '') + '" data-id="' + esc(w.id) + '" tabindex="0" aria-label="Show chart for ' + esc(w.name) + '">' +
        '<td><div class="wl-name">' + label + '</div><div class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</div>' + workloadNotes(w) + '</td>' +
        '<td class="num">' + ui.money(w.annualSpend, approx) + '</td>' +
        '<td class="num strong">' + ui.money(w.annualSavings, approx) + '</td>' +
        '<td class="num"><span class="bar"><i style="width:' + Math.round(w.savingsPct / maxPct * 100) + '%"></i></span>' + ui.pct(w.savingsPct) + '</td>' +
        '<td class="num">' + ui.num(w.wasteCores, 2) + '</td>' +
        '<td class="num">' + ui.num(w.wasteGiB, 2) + ' GiB</td></tr>';
    }).join('');
    body += rest.map(function (w) {
      return '<tr class="excluded" data-id="' + esc(w.id) + '"><td><div class="wl-name">' + esc(w.name) + '</div><div class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</div></td>' +
        '<td colspan="5" class="muted"><span class="pill pill-caution">No estimate</span> ' + esc(w.reason) + '</td></tr>';
    }).join('');
    host.querySelector('table').innerHTML = '<thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody>';
  }

  function bind(host, ctx) {
    var toggle = function (tr) {
      if (!tr || tr.classList.contains('excluded')) return;
      var id = tr.getAttribute('data-id');
      ctx.set({ workload: id === ctx.state.workload ? null : id });
    };
    host.querySelector('.tabs').addEventListener('click', function (e) {
      var b = e.target.closest('button[data-mode]');
      if (!b) return;
      view.mode = b.getAttribute('data-mode');
      renderChart(host, ctx);
    });
    host.querySelector('.wl-back').addEventListener('click', function () { ctx.set({ workload: null }); });
    host.querySelector('table').addEventListener('click', function (e) {
      var sortBtn = e.target.closest('button[data-sort]');
      if (sortBtn) {
        var key = sortBtn.getAttribute('data-sort');
        view.sort = { key: key, dir: view.sort.key === key ? -view.sort.dir : (key === 'name' ? 1 : -1) };
        renderTable(host, ctx);
        return;
      }
      toggle(e.target.closest('tr[data-id]'));
    });
    host.querySelector('table').addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      var tr = e.target.closest('tr[data-id]');
      if (tr && e.target === tr) { e.preventDefault(); toggle(tr); }
    });
  }

  function render(host, ctx) {
    // The handlers bound below read the latest context through host.ctx.
    Object.assign(host.ctx || (host.ctx = {}), ctx);
    if (!host.querySelector('.wl-chart')) {
      host.innerHTML =
        '<div class="card"><div class="card-head"><div><h2 class="wl-chart-title"></h2><div class="sub wl-chart-sub"></div></div>' +
        '<div class="card-tools"><button type="button" class="link wl-back" hidden>← Whole cluster</button><div class="tabs" role="tablist"></div></div></div>' +
        '<div class="legend"></div><div class="chart-host wl-chart"></div></div>' +
        '<div class="card"><div class="card-head"><div><h2>All workloads</h2><div class="sub">Sorted by waste. Select a workload to chart it.</div></div></div>' +
        '<div class="table-wrap"><table></table></div></div>';
      bind(host, host.ctx);
    }
    renderChart(host, host.ctx);
    renderTable(host, host.ctx);
  }

  SP.sections.register({
    id: 'workloads', number: 3, title: 'Workloads', render: render,
    resize: function (host) { if (host.ctx) renderChart(host, host.ctx); }
  });
})();
