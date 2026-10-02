/* Section 3: workloads. The list of every workload in the file, and the
 * drill-down for the workload in focus: its time chart, the per-pod requests
 * Thoras would set and what could make its saving smaller or unsafe.
 * The figures and markup are plain functions, so they also load under node. */
(function () {
  'use strict';

  var inNode = typeof module !== 'undefined' && module.exports;
  var ui = inNode ? require('./components.js') : SP.ui;
  var chart = inNode ? require('./chart.js') : SP.chart;
  var model = inNode ? require('./model.js') : SP.model;
  var state = inNode ? require('./state.js') : SP.state;
  var esc = ui.esc;

  var PAGE_SIZE = 50;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  var COLUMNS = [
    { key: 'name', label: 'Workload' },
    { key: 'status', label: 'Status' },
    { key: 'annualSpend', label: 'Annual spend', num: true },
    { key: 'annualSavings', label: 'Annual savings', num: true },
    { key: 'savingsPct', label: 'Waste', num: true },
    { key: 'wasteCores', label: 'CPU to free', num: true },
    { key: 'wasteGiB', label: 'Memory to free', num: true },
    { key: 'replicas', label: 'Replicas', num: true },
    { key: 'oomKills', label: 'OOM kills', num: true }
  ];
  var STATUS = {
    ok: { label: 'Counted', cls: 'pill-brand', order: 0 },
    partial: { label: 'Partial data', cls: 'pill-neutral', order: 1 },
    excluded: { label: 'Not counted', cls: 'pill-outline', order: 2 }
  };

  function when(sec) {
    var d = new Date(sec * 1000), pad = function (n) { return n < 10 ? '0' + n : String(n); };
    return MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate() + ' ' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()) + ' UTC';
  }

  // Index range [lo, hi) of a prepared workload's grid inside the time range.
  function bounds(p, range) {
    var t = p.s.t;
    return range ? [model.lowerBound(t, range[0]), model.upperBound(t, range[1])] : [0, t.length];
  }

  // OOM kills inside the time range. A kill is stamped at the end of its step,
  // so one stamped at the range start belongs to the step before it, and the
  // step that ends just after the range still belongs to it.
  function oomIn(p, range, step) {
    var out = { count: 0, marks: [] }, o = p.oom;
    for (var i = 0; i < o.t.length; i++) {
      if (range && (o.t[i] <= range[0] || o.t[i] > range[1] + (step || 0))) continue;
      out.count += o.n[i];
      out.marks.push({ t: o.t[i], label: (o.n[i] === 1 ? 'OOM kill' : o.n[i] + ' OOM kills') + ', ' + when(o.t[i]) });
    }
    return out;
  }

  // Replicas, weighted by time, over the points the figures count.
  function meanReplicas(p, lo, hi) {
    var s = 0, n = 0;
    for (var k = lo; k < hi; k++) {
      if (!isFinite(p.s.reqCpu[k]) || !isFinite(p.replicas[k])) continue;
      s += p.replicas[k] * p.weight[k]; n += p.weight[k];
    }
    return n ? s / n : NaN;
  }

  var preparedById = typeof WeakMap === 'function' ? new WeakMap() : null;
  function prepared(data) {
    var map = preparedById && preparedById.get(data);
    if (!map) {
      map = {};
      data.workloads.forEach(function (p) { map[p.id] = p; });
      if (preparedById) preparedById.set(data, map);
    }
    return map;
  }

  // One row per workload in scope, with the figures model.compute gives it and
  // the replicas and OOM kills over the same time range.
  function rows(data, result) {
    var byId = prepared(data), range = result.levers.range;
    return result.workloads.map(function (w) {
      var p = byId[w.id], counted = !!w.series;
      var lohi = counted ? bounds(p, range) : null;
      return {
        id: w.id, name: w.name, ns: w.ns, kind: w.kind, status: counted ? w.status : 'excluded', reason: w.reason, notes: w.notes,
        annualSpend: w.annualSpend, annualSavings: w.annualSavings, savingsPct: w.savingsPct,
        wasteCores: w.wasteCores, wasteGiB: w.wasteGiB,
        replicas: counted ? meanReplicas(p, lohi[0], lohi[1]) : NaN,
        oomKills: oomIn(p, range, p.s ? p.s.step : 0).count
      };
    });
  }

  function cmp(a, b, key, dir) {
    if (key === 'status') return dir * (STATUS[a.status].order - STATUS[b.status].order);
    var av = a[key], bv = b[key];
    if (typeof av === 'string') return dir * av.localeCompare(bv);
    return dir * (av - bv);
  }

  // The rows to list: matching the filter, counted ones sorted, the rest after them by name.
  function arrange(list, view) {
    var q = view.filter.trim().toLowerCase(), s = view.sort;
    var hit = q ? list.filter(function (r) { return r.name.toLowerCase().indexOf(q) >= 0 || r.ns.toLowerCase().indexOf(q) >= 0; }) : list;
    var counted = hit.filter(function (r) { return r.status !== 'excluded'; });
    var rest = hit.filter(function (r) { return r.status === 'excluded'; });
    counted.sort(function (a, b) { return cmp(a, b, s.key, s.dir) || a.name.localeCompare(b.name) || a.ns.localeCompare(b.ns); });
    rest.sort(function (a, b) { return a.name.localeCompare(b.name) || a.ns.localeCompare(b.ns); });
    return counted.concat(rest);
  }

  // The drill-down figures for one workload under the current levers. Per-pod
  // values divide each step's total by its replicas.
  function detail(data, result, id) {
    var w = result.workloads.filter(function (x) { return x.id === id; })[0];
    if (!w) return null;
    var p = prepared(data)[id], lv = result.levers;
    var d = { w: w, p: p, containers: p.containers, oom: oomIn(p, lv.range, p.s ? p.s.step : 0), counted: !!w.series };
    if (!d.counted) return d;
    var s = p.s, lohi = bounds(p, lv.range), lo = lohi[0], hi = lohi[1];
    var sw = 0, swPod = 0, use = [0, 0], now = [0, 0], tgt = [0, 0], lim = [0, 0], peak = [0, 0], safe = [0, 0], limW = [0, 0];
    for (var k = lo; k < hi; k++) {
      if (!isFinite(s.reqCpu[k])) continue;
      var wt = p.weight[k], rep = p.replicas[k];
      sw += wt;
      use[0] += s.useCpu[k] * wt; use[1] += s.useMem[k] * wt;
      peak[0] = Math.max(peak[0], s.peakCpu[k]); peak[1] = Math.max(peak[1], s.peakMem[k]);
      if (rep > 0) {
        swPod += wt;
        now[0] += s.reqCpu[k] / rep * wt; now[1] += s.reqMem[k] / rep * wt;
        tgt[0] += s.tgtCpu[k] / rep * wt; tgt[1] += s.tgtMem[k] / rep * wt;
        safe[0] = Math.max(safe[0], s.peakCpu[k] / rep); safe[1] = Math.max(safe[1], s.peakMem[k] / rep);
      }
      if (p.limCpu && isFinite(p.limCpu[k])) { lim[0] += p.limCpu[k] * wt; limW[0] += wt; }
      if (p.limMem && isFinite(p.limMem[k])) { lim[1] += p.limMem[k] * wt; limW[1] += wt; }
    }
    var per = function (v) { return sw ? v / sw : NaN; };
    // Time scaled to zero has no pods, so it does not count toward per-pod values.
    var perPod = function (v) { return swPod ? v / swPod : NaN; };
    d.replicas = meanReplicas(p, lo, hi);
    d.cpu = {
      requested: w.reqCores, mean: per(use[0]), peak: peak[0],
      now: perPod(now[0]), target: perPod(tgt[0]), peakSafe: safe[0] * lv.headroom, limit: limW[0] ? lim[0] / limW[0] : null
    };
    d.mem = {
      requested: w.reqGiB, mean: per(use[1]), peak: peak[1],
      now: perPod(now[1]), target: perPod(tgt[1]), peakSafe: safe[1] * lv.headroom, limit: limW[1] ? lim[1] / limW[1] : null
    };
    return d;
  }

  // Two significant figures below 0.01, so a polled sidecar's 0.0021 cores does not read as 0.
  function qty(v) { return v > 0 && v < 0.01 ? String(Number(v.toPrecision(2))) : ui.amount(v); }
  function cores(v) { return qty(v) + ' ' + (Math.abs(v - 1) < 0.005 ? 'core' : 'cores'); }
  function gib(v) { return qty(v) + ' GiB'; }

  // What could make the saving smaller or unsafe, most serious first.
  function risks(d, fidelity) {
    var out = [], c = d.cpu, m = d.mem;
    if (d.oom.count) {
      var n = d.oom.count + ' OOM ' + ui.plural(d.oom.count, 'kill') + ' in the window. ';
      out.push({
        tone: 'danger',
        text: m.limit !== null ?
          n + 'Memory is already tight at the current limit, so lower the memory request only with the limit raised or the cause fixed.' :
          n + 'No memory limit is set, so the node ran short of memory. Lower the memory request only once the cause is fixed.'
      });
    }
    if (m.limit !== null && m.limit < m.peakSafe) {
      out.push({ tone: 'caution', text: 'The memory limit (' + gib(m.limit) + ' per pod) is below the peak-safe value (' + gib(m.peakSafe) + '). Thoras would raise the limit with the request.' });
    }
    if (c.limit !== null && c.limit < c.peakSafe) {
      out.push({ tone: 'caution', text: 'The CPU limit (' + cores(c.limit) + ' per pod) is below the peak-safe value (' + cores(c.peakSafe) + '). CPU above the limit is throttled, so the measured peaks, and the target, may be low.' });
    }
    if (ui.confidenceLevel(fidelity) === 'provisional') {
      out.push({ tone: 'caution', text: (fidelity.snapshot ? 'A single snapshot' : fidelity.label) + ' cannot show daily or weekly peaks, so this saving can move a lot with more data.' });
    }
    if (d.w.status === 'partial') out.push({ tone: 'info', text: d.w.reason + ': ' + d.containers.filter(function (x) { return x.excluded; }).map(function (x) { return x.name; }).join(', ') + '.' });
    d.w.notes.forEach(function (key) { out.push({ tone: 'info', text: model.notes[key].workload + '.' }); });
    return out;
  }

  function link(st, id, text, cls) {
    var href = '#' + state.serialize(Object.assign({}, st, { section: 'workloads', workload: id }));
    return '<a href="' + esc(href) + '" data-workload="' + esc(id) + '"' + (cls ? ' class="' + cls + '"' : '') + '>' + text + '</a>';
  }

  function statusPill(status) {
    return '<span class="pill ' + STATUS[status].cls + '">' + STATUS[status].label + '</span>';
  }

  function oomCell(n) {
    return n ? '<span class="pill pill-danger">' + ui.num(n, 0) + '</span>' : '0';
  }

  function listHead(list, total) {
    var counted = list.filter(function (r) { return r.status !== 'excluded'; }).length;
    return counted + ' of ' + total + ' ' + ui.plural(total, 'workload') + ' counted';
  }

  function sortNote(view) {
    var col = COLUMNS.filter(function (c) { return c.key === view.sort.key; })[0];
    return 'Sorted by ' + col.label.toLowerCase() + '. Select a workload to see its history.';
  }

  // maxPct scales the waste bars, so a share draws the same length on every page.
  function tableHtml(page, view, ctx, maxPct) {
    var s = view.sort, approx = ctx.approx, st = ctx.state, last = view.last;
    var head = COLUMNS.map(function (c) {
      var active = s.key === c.key;
      return '<th scope="col"' + (c.num ? ' class="num"' : '') + ' aria-sort="' + (active ? (s.dir < 0 ? 'descending' : 'ascending') : 'none') + '">' +
        '<button type="button" data-sort="' + c.key + '">' + c.label + (active ? (s.dir < 0 ? ' ↓' : ' ↑') : '') + '</button></th>';
    }).join('');
    var body = page.map(function (r) {
      var name = '<td>' + link(st, r.id, esc(r.name), 'wl-link') + '<div class="wl-sub">' + esc(r.ns) + ' · ' + esc(r.kind) + '</div></td>';
      if (r.status === 'excluded') {
        return '<tr class="excluded" data-id="' + esc(r.id) + '">' + name +
          '<td>' + statusPill('excluded') + ' <span class="wl-reason">' + esc(r.reason) + '</span></td>' +
          '<td class="num">–</td><td class="num">–</td><td class="num">–</td><td class="num">–</td><td class="num">–</td><td class="num">–</td>' +
          '<td class="num">' + (r.oomKills ? oomCell(r.oomKills) : '–') + '</td></tr>';
      }
      return '<tr data-id="' + esc(r.id) + '"' + (r.id === last ? ' class="selected"' : '') + '>' + name +
        '<td>' + statusPill(r.status) + '</td>' +
        '<td class="num">' + ui.money(r.annualSpend, approx) + '</td>' +
        '<td class="num strong">' + ui.money(r.annualSavings, approx) + '</td>' +
        '<td class="num"><span class="bar"><i style="width:' + Math.round(r.savingsPct / maxPct * 100) + '%"></i></span>' + ui.pct(r.savingsPct) + '</td>' +
        '<td class="num">' + ui.amount(r.wasteCores) + '</td>' +
        '<td class="num">' + ui.amount(r.wasteGiB) + ' GiB</td>' +
        '<td class="num">' + ui.num(r.replicas, 1) + '</td>' +
        '<td class="num">' + oomCell(r.oomKills) + '</td></tr>';
    }).join('');
    if (!page.length) body = '<tr><td colspan="' + COLUMNS.length + '" class="empty">No workload matches “' + esc(view.filter.trim()) + '”.</td></tr>';
    return '<thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody>';
  }

  // The list as HTML: heading, table and pager for one page of the arranged rows.
  function listParts(all, view, ctx) {
    var list = arrange(all, view);
    var pages = Math.max(1, Math.ceil(list.length / PAGE_SIZE));
    view.page = Math.min(Math.max(0, view.page), pages - 1);
    var from = view.page * PAGE_SIZE, page = list.slice(from, from + PAGE_SIZE);
    var range = list.length ? (from + 1) + '–' + (from + page.length) + ' of ' + ui.num(list.length, 0) : '0 of ' + ui.num(all.length, 0);
    return {
      title: listHead(all, all.length),
      sub: sortNote(view),
      table: tableHtml(page, view, ctx, list.reduce(function (m, r) { return r.status !== 'excluded' ? Math.max(m, r.savingsPct) : m; }, 0.0001)),
      range: range,
      prev: view.page > 0,
      next: view.page < pages - 1
    };
  }

  function figure(label, value, sep) {
    return '<div class="wl-fig' + (sep ? ' sep' : '') + '"><div class="wl-fig-label">' + esc(label) + '</div><div class="wl-fig-value">' + value + '</div></div>';
  }

  function limitsText(c, m) {
    if (c.limit === null && m.limit === null) return 'None set';
    return (c.limit === null ? 'No CPU limit' : cores(c.limit)) + ' · ' + (m.limit === null ? 'no memory limit' : gib(m.limit));
  }

  function detailHead(d, ctx) {
    var w = d.w, names = d.containers.map(function (c) { return c.name; });
    return '<div class="wl-head"><div><a href="#' + esc(state.serialize(Object.assign({}, ctx.state, { workload: null, section: 'workloads' }))) + '" class="wl-back">← All workloads</a>' +
      '<h3 class="wl-title">' + esc(w.name) + '</h3><div class="sub">' + esc(w.ns) + ' · ' + esc(w.kind) + ' · ' + esc(names.join(', ')) + '</div></div>' +
      '<div class="wl-pills">' + statusPill(d.counted ? w.status : 'excluded') + ' ' + ui.confidencePill(ui.confidenceLevel(ctx.result.fidelity)) + '</div></div>';
  }

  function figuresCard(d, approx) {
    var w = d.w, c = d.cpu, m = d.mem;
    return '<div class="card wl-figs' + (approx ? ' wl-figs-provisional' : '') + '">' +
      figure('Annual spend', esc(ui.money(w.annualSpend, approx))) +
      figure('Annual savings', esc(ui.money(w.annualSavings, approx)) + ' · ' + ui.pct(w.savingsPct)) +
      figure('Replicas', esc(ui.num(d.replicas, 1)), true) +
      figure('CPU requested', esc(cores(c.requested))) +
      figure('CPU used, mean / peak', esc(qty(c.mean) + ' / ' + cores(c.peak))) +
      figure('Memory requested', esc(gib(m.requested)), true) +
      figure('Memory used, mean / peak', esc(qty(m.mean) + ' / ' + gib(m.peak))) +
      figure('OOM kills', oomCell(d.oom.count), true) +
      figure('Limits per pod', esc(limitsText(c, m))) +
      '</div>';
  }

  var DETAIL_MODES = [['cpu', 'CPU'], ['mem', 'Memory'], ['spend', 'Spend']];

  function legendItems(mode, lv, oom) {
    var pct = Math.round((lv.headroom - 1) * 100);
    var items = mode === 'spend' ?
      [['a', 'Current spend'], ['b', 'Without waste'], ['gap', 'Waste']] :
      [['a', 'Requested'], ['u', 'Usage (thin line: peak in step)'], ['b', 'Target: peak + ' + pct + '%'], ['gap', 'Waste']];
    if (oom && mode !== 'spend') items.push(['oom', 'OOM kill']);
    return items;
  }

  function chartCard(d, view, ctx) {
    var lv = ctx.result.levers, pct = Math.round((lv.headroom - 1) * 100), mins = Math.round(lv.peakWindow / 60);
    var tabs = DETAIL_MODES.map(function (m) {
      return '<button type="button" role="tab" aria-selected="' + (m[0] === view.mode) + '" data-mode="' + m[0] + '">' + m[1] + '</button>';
    }).join('');
    var legend = legendItems(view.mode, lv, d.oom.marks.length).map(function (it) {
      return '<span><i class="swatch swatch-' + it[0] + '"></i>' + esc(it[1]) + '</span>';
    }).join('');
    return '<div class="card"><div class="card-head"><div><h2>Requested, usage and target</h2>' +
      '<div class="sub">Totals across replicas. The target is the highest usage in the last ' + mins + ' minutes plus ' + pct + '%; the hatched gap is waste.</div></div>' +
      '<div class="tabs" role="tablist" aria-label="Chart view">' + tabs + '</div></div>' +
      '<div class="legend wl-legend">' + legend + '</div><div class="chart-host wl-chart"></div></div>';
  }

  function settingsCard(d, ctx) {
    var c = d.cpu, m = d.mem, lv = ctx.result.levers;
    var pct = Math.round((lv.headroom - 1) * 100), mins = Math.round(lv.peakWindow / 60);
    var row = function (label, r, fmt) {
      return '<tr><td>' + label + '</td><td class="num">' + esc(fmt(r.now)) + '</td><td class="num">' + esc(fmt(r.target)) + '</td><td class="num">' + esc(fmt(r.peakSafe)) + '</td></tr>';
    };
    return '<div class="card"><h2>What Thoras would set</h2><div class="sub">Per-pod requests, if the window repeated</div>' +
      '<div class="table-wrap"><table class="table-plain"><thead><tr><th scope="col">Per pod</th><th scope="col" class="num">Now</th><th scope="col" class="num">Thoras target</th><th scope="col" class="num">Peak-safe</th></tr></thead><tbody>' +
      row('CPU request', c, qty) + row('Memory request', m, gib) + '</tbody></table></div>' +
      '<p class="wl-note">Thoras adjusts requests continuously. “Thoras target” is what the request would have averaged over the window; “peak-safe” is one fixed value that covers the highest ' +
      mins + '-minute peak plus ' + pct + '%.</p></div>';
  }

  function risksCard(d, ctx) {
    var list = risks(d, ctx.result.fidelity);
    var body = list.length ? list.map(function (r) { return '<div class="risk risk-' + r.tone + '">' + esc(r.text) + '</div>'; }).join('') :
      '<p class="muted">No OOM kills and no limit below the peak-safe value in this window.</p>';
    return '<div class="card"><h2>Risk read</h2><div class="sub">What could make the saving smaller or unsafe</div><div class="risks">' + body + '</div></div>';
  }

  function excludedCard(d) {
    var rows = d.containers.map(function (c) {
      return '<tr><td>' + esc(c.name) + '</td><td>' + (c.missing.length ? esc(c.missing.join(', ')) : '<span class="muted">Complete</span>') + '</td></tr>';
    }).join('');
    return ui.banner({ tone: 'caution', title: 'Not counted.', body: esc(d.w.reason) + '. This workload is listed but left out of the totals.' }) +
      '<div class="card"><h2>Series by container</h2><div class="sub">The series each container is missing</div>' +
      '<div class="table-wrap"><table class="table-plain"><thead><tr><th scope="col">Container</th><th scope="col">Missing</th></tr></thead><tbody>' + rows + '</tbody></table></div>' +
      (d.oom.count ? '<p class="wl-note">' + d.oom.count + ' OOM ' + ui.plural(d.oom.count, 'kill') + ' in the window.</p>' : '') + '</div>';
  }

  function detailHtml(d, view, ctx) {
    if (!d.counted) return detailHead(d, ctx) + excludedCard(d);
    return detailHead(d, ctx) + figuresCard(d, ctx.approx) + chartCard(d, view, ctx) +
      '<div class="pair">' + settingsCard(d, ctx) + risksCard(d, ctx) + '</div>';
  }

  var api = { rows: rows, arrange: arrange, detail: detail, risks: risks, listParts: listParts, detailHtml: detailHtml, PAGE_SIZE: PAGE_SIZE };
  if (inNode) { module.exports = api; return; }

  // Browser: the section's view state, which stays on the page and out of the link.
  var view = { sort: { key: 'annualSavings', dir: -1 }, filter: '', page: 0, mode: 'cpu', last: null };
  var cache = { result: null, rows: null };

  function currentRows(ctx) {
    if (cache.result !== ctx.result) cache = { result: ctx.result, rows: rows(ctx.data, ctx.result) };
    return cache.rows;
  }

  function renderList(host, ctx) {
    var parts = listParts(currentRows(ctx), view, ctx);
    host.querySelector('.wl-list-title').textContent = parts.title;
    host.querySelector('.wl-list-sub').textContent = parts.sub;
    host.querySelector('.wl-table').innerHTML = parts.table;
    host.querySelector('.wl-range').textContent = parts.range;
    host.querySelector('[data-page="prev"]').disabled = !parts.prev;
    host.querySelector('[data-page="next"]').disabled = !parts.next;
  }

  function renderChart(host, ctx, d) {
    var el = host.querySelector('.wl-chart');
    if (!el || !d || !d.counted) return;
    var lv = ctx.result.levers;
    var names = view.mode === 'spend' ? null : { b: 'Target: peak + ' + Math.round((lv.headroom - 1) * 100) + '%' };
    chart.render(el, d.w.series, view.mode, {
      title: d.w.name, fromZero: true, className: 'chart-detail', names: names,
      markers: view.mode === 'spend' ? [] : d.oom.marks
    });
  }

  function open(ctx, id) {
    ctx.set({ workload: id });
    ctx.go('workloads');
  }

  function bind(host) {
    host.addEventListener('click', function (e) {
      var ctx = host.ctx;
      var a = e.target.closest('a[data-workload]');
      if (a) { e.preventDefault(); open(ctx, a.getAttribute('data-workload')); return; }
      if (e.target.closest('.wl-back')) { e.preventDefault(); view.last = ctx.state.workload; open(ctx, null); return; }
      var sortBtn = e.target.closest('button[data-sort]');
      if (sortBtn) {
        var key = sortBtn.getAttribute('data-sort');
        view.sort = { key: key, dir: view.sort.key === key ? -view.sort.dir : (key === 'name' || key === 'status' ? 1 : -1) };
        view.page = 0;
        renderList(host, ctx);
        return;
      }
      var pageBtn = e.target.closest('button[data-page]');
      if (pageBtn) { view.page += pageBtn.getAttribute('data-page') === 'next' ? 1 : -1; renderList(host, ctx); return; }
      var tab = e.target.closest('button[data-mode]');
      if (tab) { view.mode = tab.getAttribute('data-mode'); render(host, ctx); return; }
      // A click anywhere on a row opens the workload, as its name link does.
      var tr = e.target.closest('.wl-table tbody tr[data-id]');
      if (tr && !window.getSelection().toString()) open(ctx, tr.getAttribute('data-id'));
    });
    host.querySelector('.wl-filter').addEventListener('input', function (e) {
      view.filter = e.target.value;
      view.page = 0;
      renderList(host, host.ctx);
    });
  }

  function render(host, ctx) {
    // The handlers bound below read the latest context through host.ctx.
    host.ctx = ctx;
    if (!host.querySelector('.wl-list')) {
      host.innerHTML =
        '<div class="wl-list"><p class="wl-intro">Every workload in the file. Waste is the request above the target; a workload missing a required series is listed but not counted.</p>' +
        '<div class="card"><div class="card-head"><div><h2 class="wl-list-title"></h2><div class="sub wl-list-sub"></div></div>' +
        '<label class="wl-search"><span class="visually-hidden">Filter workloads</span><input type="search" class="wl-filter" placeholder="Filter by name or namespace" autocomplete="off"></label></div>' +
        '<div class="table-wrap"><table class="wl-table"></table></div>' +
        '<div class="wl-pager"><span class="wl-range"></span><span class="wl-pager-buttons"><button type="button" class="pager" data-page="prev">Previous</button><button type="button" class="pager" data-page="next">Next</button></span></div></div></div>' +
        '<div class="wl-detail" hidden></div>';
      bind(host);
    }
    var d = ctx.state.workload ? detail(ctx.data, ctx.result, ctx.state.workload) : null;
    host.querySelector('.wl-list').hidden = !!d;
    var box = host.querySelector('.wl-detail');
    box.hidden = !d;
    if (d) {
      box.innerHTML = detailHtml(d, view, ctx);
      renderChart(box, ctx, d);
    } else {
      box.innerHTML = '';
      renderList(host, ctx);
    }
  }

  SP.sections.register({
    id: 'workloads', number: 3, title: 'Workloads', render: render,
    resize: function (host) {
      if (!host.ctx || !host.ctx.state.workload) return;
      renderChart(host, host.ctx, detail(host.ctx.data, host.ctx.result, host.ctx.state.workload));
    }
  });
})();
