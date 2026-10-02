/* Page wiring: loads the contract file, runs the model, renders tiles, chart and table. */
(function () {
  'use strict';

  var model = SP.model, chart = SP.chart;
  var DATA_URL = new URLSearchParams(location.search).get('data') || 'savings-preview.otlp.json';
  var state = { result: null, selected: null, mode: 'spend', sort: { key: 'annualSavings', dir: -1 } };

  function $(id) { return document.getElementById(id); }
  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; });
  }

  function applyTheme() {
    var dark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  }

  function money(v) {
    if (Math.abs(v) >= 1e6) return '$' + (v / 1e6).toFixed(2) + 'M';
    return '$' + Math.round(v).toLocaleString('en-US');
  }

  function num(v, digits) {
    return v.toLocaleString('en-US', { maximumFractionDigits: digits === undefined ? 1 : digits });
  }

  function showError(message) {
    $('loading').hidden = true;
    $('report').hidden = true;
    $('start').hidden = false;
    $('error').textContent = message || '';
    $('error').hidden = !message;
  }

  function load(doc) {
    var result;
    try { result = model.analyze(doc); } catch (e) { showError(e.message); return; }
    state.result = result;
    state.selected = null;
    $('loading').hidden = true;
    $('start').hidden = true;
    $('report').hidden = false;
    renderAll();
  }

  function fetchData() {
    $('loading').hidden = false;
    fetch(DATA_URL).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(load).catch(function () {
      showError('');
    });
  }

  function readFile(file) {
    var reader = new FileReader();
    reader.onload = function () {
      var doc;
      try { doc = JSON.parse(reader.result); } catch (e) { showError('That file is not valid JSON.'); return; }
      load(doc);
    };
    reader.readAsText(file);
  }

  function renderAll() {
    var r = state.result;
    $('cluster').textContent = r.cluster || 'Unnamed cluster';
    renderFidelity();
    renderTiles();
    renderNotes();
    renderChart();
    renderTable();
  }

  function renderFidelity() {
    var f = state.result.fidelity;
    var tierText = { 1: 'Full history', 2: 'Reduced resolution', 3: 'Limited history' }[f.tier] || '';
    $('fidelity').innerHTML = '<span class="badge tier-' + f.tier + '">Tier ' + f.tier + '</span> ' +
      '<span class="fidelity-label">' + esc(f.label) + '</span>' +
      '<span class="fidelity-detail">' + esc(tierText) + (f.source ? ' · ' + esc(f.source) : '') + '</span>';
  }

  function tile(label, value, sub) {
    return '<div class="tile"><div class="tile-label">' + esc(label) + '</div><div class="tile-value">' + esc(value) +
      '</div><div class="tile-sub">' + esc(sub) + '</div></div>';
  }

  function renderTiles() {
    var t = state.result.totals;
    var pct = t.spend > 0 ? Math.round(t.savings / t.spend * 100) : 0;
    $('tiles').innerHTML =
      tile('Projected annual spend', money(t.spend), 'Priced from current requests') +
      tile('Annual savings opportunity', money(t.savings), pct + '% of projected spend') +
      tile('CPU cores that could be freed', num(t.cores), 'of ' + num(t.reqCores) + ' requested') +
      tile('Memory that could be freed', num(t.gib) + ' GiB', 'of ' + num(t.reqGiB) + ' GiB requested');
  }

  function renderNotes() {
    var r = state.result, t = r.totals, notes = [];
    if (t.counted < t.workloads) {
      var left = r.workloads.filter(function (w) { return !w.series; }).length;
      notes.push('<strong>' + left + ' of ' + t.workloads + ' workloads</strong> have too little data for a savings figure and are not in the totals.');
    }
    var partial = r.workloads.filter(function (w) { return w.status === 'partial'; }).length;
    if (partial) notes.push('<strong>' + partial + '</strong> workloads have containers with missing data. Those containers are left out of the workload figures.');
    $('notes').innerHTML = notes.map(function (n) { return '<p>' + n + '</p>'; }).join('');
    $('notes').hidden = !notes.length;
    $('basis').textContent = 'Priced with ' + r.pricing.basis + '. Target is 1.1 times the highest usage in any 15-minute window. Waste is the amount requested above the target.';
  }

  function selectedWorkload() {
    if (!state.selected) return null;
    return state.result.workloads.filter(function (w) { return w.id === state.selected; })[0] || null;
  }

  function renderChart() {
    var w = selectedWorkload();
    var r = state.result;
    var series = w ? w.series : r.series;
    $('chart-title').textContent = w ? w.name : 'Whole cluster';
    $('chart-sub').textContent = w ? w.ns + ' · ' + w.kind : r.totals.counted + ' workloads';
    $('back').hidden = !w;
    var tabs = $('tabs');
    tabs.innerHTML = Object.keys(chart.modes).map(function (k) {
      return '<button type="button" role="tab" aria-selected="' + (k === state.mode) + '" data-mode="' + k + '">' + chart.modes[k].label + '</button>';
    }).join('');
    var mode = chart.modes[state.mode];
    $('legend').innerHTML = '<span><i class="swatch swatch-a"></i>' + esc(mode.aName) + '</span>' +
      '<span><i class="swatch swatch-b"></i>' + esc(mode.bName) + '</span>' +
      '<span><i class="swatch swatch-gap"></i>' + (mode.money ? 'Waste' : 'Gap') + '</span>';
    var host = $('chart');
    if (!series) { host.innerHTML = '<p class="empty">No workload has enough data to chart.</p>'; return; }
    chart.render(host, series, state.mode, { title: w ? w.name : 'Whole cluster' });
  }

  function cmp(a, b, key, dir) {
    var av = a[key], bv = b[key];
    if (typeof av === 'string') return dir * av.localeCompare(bv);
    return dir * (av - bv);
  }

  var COLUMNS = [
    { key: 'name', label: 'Workload', num: false },
    { key: 'annualSpend', label: 'Annual spend', num: true },
    { key: 'annualSavings', label: 'Annual savings', num: true },
    { key: 'savingsPct', label: 'Waste', num: true },
    { key: 'wasteCores', label: 'CPU to free', num: true },
    { key: 'wasteGiB', label: 'Memory to free', num: true }
  ];

  function renderTable() {
    var s = state.sort;
    var rows = state.result.workloads.slice();
    var ok = rows.filter(function (w) { return w.series; }).sort(function (a, b) { return cmp(a, b, s.key, s.dir) || a.name.localeCompare(b.name); });
    var rest = rows.filter(function (w) { return !w.series; }).sort(function (a, b) { return a.name.localeCompare(b.name); });
    var head = COLUMNS.map(function (c) {
      var active = s.key === c.key;
      return '<th scope="col" class="' + (c.num ? 'num' : '') + '" aria-sort="' + (active ? (s.dir < 0 ? 'descending' : 'ascending') : 'none') + '">' +
        '<button type="button" data-sort="' + c.key + '">' + c.label + (active ? (s.dir < 0 ? ' ↓' : ' ↑') : '') + '</button></th>';
    }).join('');
    var maxPct = Math.max.apply(null, [0.0001].concat(ok.map(function (w) { return w.savingsPct; })));
    var body = ok.map(function (w) {
      var label = esc(w.name) + (w.status === 'partial' ? ' <span class="chip" title="' + esc(w.reason) + '">Partial data</span>' : '');
      return '<tr class="' + (w.id === state.selected ? 'selected' : '') + '" data-id="' + esc(w.id) + '" tabindex="0" aria-label="Show chart for ' + esc(w.name) + '">' +
        '<td><div class="wl-name">' + label + '</div><div class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</div></td>' +
        '<td class="num">' + money(w.annualSpend) + '</td>' +
        '<td class="num strong">' + money(w.annualSavings) + '</td>' +
        '<td class="num"><span class="bar"><i style="width:' + Math.round(w.savingsPct / maxPct * 100) + '%"></i></span>' + Math.round(w.savingsPct * 100) + '%</td>' +
        '<td class="num">' + num(w.wasteCores, 2) + '</td>' +
        '<td class="num">' + num(w.wasteGiB, 2) + ' GiB</td></tr>';
    }).join('');
    body += rest.map(function (w) {
      return '<tr class="excluded" data-id="' + esc(w.id) + '"><td><div class="wl-name">' + esc(w.name) + '</div><div class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</div></td>' +
        '<td colspan="5" class="muted"><span class="chip warn">No estimate</span> ' + esc(w.reason) + '</td></tr>';
    }).join('');
    $('table').innerHTML = '<thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody>';
  }

  function select(id) {
    state.selected = id;
    renderChart();
    renderTable();
    $('chart-card').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  function bind() {
    $('tabs').addEventListener('click', function (e) {
      var b = e.target.closest('button[data-mode]');
      if (!b) return;
      state.mode = b.getAttribute('data-mode');
      renderChart();
    });
    $('back').addEventListener('click', function () { select(null); });
    $('table').addEventListener('click', function (e) {
      var sortBtn = e.target.closest('button[data-sort]');
      if (sortBtn) {
        var key = sortBtn.getAttribute('data-sort');
        state.sort = { key: key, dir: state.sort.key === key ? -state.sort.dir : (key === 'name' ? 1 : -1) };
        renderTable();
        return;
      }
      var tr = e.target.closest('tr[data-id]');
      if (tr && !tr.classList.contains('excluded')) select(tr.getAttribute('data-id') === state.selected ? null : tr.getAttribute('data-id'));
    });
    $('table').addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      var tr = e.target.closest('tr[data-id]');
      if (tr && e.target === tr && !tr.classList.contains('excluded')) {
        e.preventDefault();
        select(tr.getAttribute('data-id') === state.selected ? null : tr.getAttribute('data-id'));
      }
    });
    $('file').addEventListener('change', function (e) { if (e.target.files[0]) readFile(e.target.files[0]); });
    ['dragover', 'drop'].forEach(function (ev) {
      document.addEventListener(ev, function (e) {
        e.preventDefault();
        if (ev === 'drop' && e.dataTransfer.files[0]) readFile(e.dataTransfer.files[0]);
      });
    });
    var timer;
    window.addEventListener('resize', function () {
      clearTimeout(timer);
      timer = setTimeout(function () { if (state.result && !$('report').hidden) renderChart(); }, 120);
    });
    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: dark)');
      if (mq.addEventListener) mq.addEventListener('change', applyTheme);
    }
  }

  applyTheme();
  bind();
  if (location.protocol === 'file:') showError('');
  else fetchData();
})();
