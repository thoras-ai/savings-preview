/* Section 2: where the money is. A treemap of savings or spend by namespace or
 * workload, then the namespaces and the workloads ranked by savings.
 * Runs in the browser, where it registers the section, and under node, where
 * it exports the functions that build the section for tests. */
(function () {
  'use strict';

  var node = typeof module !== 'undefined' && module.exports;
  var ui = node ? require('./components.js') : SP.ui, esc = ui.esc;

  var TONES = 4;           // namespaces with a colour of their own; the rest share one
  var MAP_WORKLOADS = 60;  // above this many workloads the map opens on namespaces
  var MAP_CELLS = 60;      // workload cells drawn at most; the rest share one cell
  var NAMED_GAPS = 5;      // uncounted workloads drawn one cell each, up to this many
  var NS_ROWS = 8;
  var TOP = 10;
  var MAP_HEIGHT = 380;

  function byDesc(key) {
    return function (a, b) { return b[key] - a[key] || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0); };
  }

  function lowerFirst(s) { return s ? s.charAt(0).toLowerCase() + s.slice(1) : s; }

  // Savings and spend per namespace, from the counted workloads. Namespaces
  // are ranked by savings; the first TONES get chart-1 to chart-4.
  function summarize(result) {
    var by = {}, list = [];
    result.workloads.forEach(function (w) {
      var n = by[w.ns];
      if (!n) { n = by[w.ns] = { name: w.ns, savings: 0, spend: 0, workloads: 0, counted: 0, reason: '' }; list.push(n); }
      n.workloads++;
      if (w.series) { n.counted++; n.savings += w.annualSavings; n.spend += w.annualSpend; }
      else if (!n.reason) n.reason = w.reason;
    });
    list.sort(function (a, b) { return (b.counted > 0) - (a.counted > 0) || byDesc('savings')(a, b); });
    var tone = 0, tones = {};
    list.forEach(function (n) { n.tone = n.counted && tone < TONES ? String(++tone) : 'other'; tones[n.name] = n.tone; });
    var t = result.totals;
    return { namespaces: list, tones: tones, savings: t.savings, spend: t.spend, counted: t.counted, uncounted: t.workloads - t.counted };
  }

  // Squarified treemap: lays out items (value > 0, largest first) in the
  // rectangle, keeping cells as close to square as the order allows.
  function squarify(items, w, h) {
    var total = 0, out = [];
    items.forEach(function (it) { total += it.value; });
    if (!(total > 0) || !(w > 0) || !(h > 0)) return out;
    var scale = w * h / total, x = 0, y = 0, row = [], rowSum = 0;
    function worst(sum, min, max, side) {
      var s2 = sum * sum, d2 = side * side;
      return Math.max(d2 * max / s2, s2 / (d2 * min));
    }
    function place() {
      var wide = w >= h, thick = rowSum / (wide ? h : w), at = wide ? y : x;
      row.forEach(function (it) {
        var len = it.area / thick;
        out.push(wide ? { item: it.item, x: x, y: at, w: thick, h: len } : { item: it.item, x: at, y: y, w: len, h: thick });
        at += len;
      });
      if (wide) { x += thick; w -= thick; } else { y += thick; h -= thick; }
      row = []; rowSum = 0;
    }
    var i = 0;
    while (i < items.length) {
      var a = items[i].value * scale, side = Math.min(w, h);
      if (row.length) {
        var min = row[row.length - 1].area, max = row[0].area;
        if (worst(rowSum + a, Math.min(min, a), Math.max(max, a), side) > worst(rowSum, min, max, side)) { place(); continue; }
      }
      row.push({ item: items[i], area: a });
      rowSum += a;
      i++;
    }
    if (row.length) place();
    return out;
  }

  // The level the map shows: the reader's choice, else namespaces once there
  // are more workloads than fit as cells.
  function level(result, view) {
    if (view.ns) return 'workloads';
    return view.level || (result.workloads.length > MAP_WORKLOADS ? 'namespaces' : 'workloads');
  }

  function cellNote(value, pct, metric) {
    return value + ' · ' + pct + (metric === 'spend' ? ' waste' : '');
  }

  // The map's cells, largest first. Uncounted workloads are hatched cells
  // sized at a small share of the map, as they have no figure to size them by.
  function cells(result, sum, view, approx) {
    var metric = view.metric === 'spend' ? 'annualSpend' : 'annualSavings';
    var nsMetric = view.metric === 'spend' ? 'spend' : 'savings';
    var out = [], gaps = [], total = 0;
    if (level(result, view) === 'namespaces') {
      sum.namespaces.forEach(function (n) {
        if (n.counted && n[nsMetric] > 0) {
          var m = ui.money(n[nsMetric], approx);
          out.push({ kind: 'ns', key: n.name, name: n.name, value: n[nsMetric], tone: n.tone,
            note: m + ' · ' + ui.num(n.counted, 0) + ' ' + ui.plural(n.counted, 'workload'), short: m + ' · ' + ui.num(n.counted, 0) });
        }
      });
      if (sum.uncounted) gaps.push({ name: ui.num(sum.uncounted, 0) + ' ' + ui.plural(sum.uncounted, 'workload') + ' not counted' });
    } else {
      var ws = result.workloads.filter(function (w) { return !view.ns || w.ns === view.ns; });
      var counted = ws.filter(function (w) { return w.series && w[metric] > 0; })
        .sort(function (a, b) { return b[metric] - a[metric] || a.id.localeCompare(b.id); });
      var shown = counted.length > MAP_CELLS ? counted.slice(0, MAP_CELLS - 1) : counted;
      shown.forEach(function (w) {
        out.push({ kind: 'workload', key: w.id, name: w.name, ns: w.ns, value: w[metric], tone: sum.tones[w.ns],
          note: cellNote(ui.money(w[metric], approx), ui.pct(w.savingsPct), view.metric) });
      });
      if (shown.length < counted.length) {
        var rest = counted.slice(shown.length), restValue = 0;
        rest.forEach(function (w) { restValue += w[metric]; });
        out.push({ kind: 'rest', key: 'rest', name: ui.num(rest.length, 0) + ' more workloads', value: restValue, tone: 'other', note: ui.money(restValue, approx) });
      }
      var missing = ws.filter(function (w) { return !w.series; });
      if (missing.length > NAMED_GAPS) gaps.push({ name: ui.num(missing.length, 0) + ' workloads not counted' });
      else missing.forEach(function (w) { gaps.push({ name: w.name + ' · ' + lowerFirst(w.reason) }); });
    }
    out.forEach(function (c) { total += c.value; });
    var share = total > 0 ? total * Math.min(0.02, 0.1 / Math.max(1, gaps.length)) : 1;
    gaps.forEach(function (g, i) { out.push({ kind: 'gap', key: 'gap-' + i, name: g.name, value: share, tone: 'gap' }); });
    return out;
  }

  function pc(v, of) { return Math.round(v / of * 10000) / 100 + '%'; }

  function mapHTML(list, width, height) {
    var rects = squarify(list, width, height);
    if (!rects.length) return '<p class="empty">No workload has complete data, so there is nothing to map.</p>';
    return '<div class="tm" style="height:' + height + 'px">' + rects.map(function (r) {
      var c = r.item, px = r.w, py = r.h;
      var size = px < 44 || py < 26 ? ' tm-xs' : px < 84 || py < 46 ? ' tm-s' : '';
      var pos = ' style="left:' + pc(r.x, width) + ';top:' + pc(r.y, height) + ';width:' + pc(r.w, width) + ';height:' + pc(r.h, height) + '"';
      var label = c.name + (c.note ? ', ' + c.note : '');
      if (c.kind === 'gap') return '<div class="tm-cell tm-gap' + size + '"' + pos + ' title="' + esc(c.name) + '"><span>' + esc(c.name) + '</span></div>';
      var body = '<b>' + esc(c.name) + '</b><span>' + esc(c.short && px < 190 ? c.short : c.note) + '</span>';
      var cls = 'tm-cell tm-' + c.tone + size;
      if (c.kind === 'rest') return '<div class="' + cls + '"' + pos + ' title="' + esc(label) + '">' + body + '</div>';
      var attr = c.kind === 'ns' ? ' data-ns="' + esc(c.key) + '" aria-label="' + esc(label + '. Show its workloads') + '"' :
        ' data-workload="' + esc(c.key) + '" aria-label="' + esc(label + '. Show in Workloads') + '"';
      return '<button type="button" class="' + cls + '"' + pos + attr + ' title="' + esc(label) + '">' + body + '</button>';
    }).join('') + '</div>';
  }

  function tabs(name, options, current) {
    return '<div class="tabs" role="tablist">' + options.map(function (o) {
      return '<button type="button" role="tab" aria-selected="' + (o[0] === current) + '" data-' + name + '="' + o[0] + '">' + o[1] + '</button>';
    }).join('') + '</div>';
  }

  function legend(sum, list) {
    var keys = sum.namespaces.filter(function (n) { return n.tone !== 'other'; }).map(function (n) { return [n.tone, n.name]; });
    if (list.some(function (c) { return c.tone === 'other'; })) keys.push(['other', 'other namespaces']);
    if (list.some(function (c) { return c.kind === 'gap'; })) keys.push(['gap', 'not counted']);
    return '<div class="legend">' + keys.map(function (k) { return '<span><i class="tm-key tm-' + k[0] + '"></i>' + esc(k[1]) + '</span>'; }).join('') + '</div>';
  }

  function mapCard(result, sum, view, approx, filtered, width) {
    var lvl = level(result, view), list = cells(result, sum, view, approx);
    var spend = view.metric === 'spend', what = spend ? 'Spend' : 'Savings';
    var title = view.ns ? what + ' in ' + view.ns : what + ' by ' + (lvl === 'namespaces' ? 'namespace' : 'workload');
    var counted = sum.counted, uncounted = sum.uncounted, value = spend ? sum.spend : sum.savings;
    if (view.ns) {
      var n = sum.namespaces.filter(function (x) { return x.name === view.ns; })[0];
      counted = n.counted; uncounted = n.workloads - n.counted; value = spend ? n.spend : n.savings;
    }
    var sub = ui.money(value, approx) + ' a year' + (spend ? ' of spend' : '') + ' across ' + ui.num(counted, 0) + ' counted ' + ui.plural(counted, 'workload') +
      (uncounted ? '; ' + ui.num(uncounted, 0) + ' not counted.' : '.');
    return '<div class="card"><div class="card-head"><div><h2>' + esc(title) +
      (filtered ? ' <span class="pill pill-neutral">Filtered</span>' : '') + '</h2><div class="sub">' + esc(sub) + '</div></div>' +
      '<div class="card-tools">' + (view.ns ? '<button type="button" class="link" data-map-back>← All namespaces</button>' : '') +
      tabs('level', [['namespaces', 'Namespaces'], ['workloads', 'Workloads']], view.ns ? '' : lvl) +
      tabs('metric', [['savings', 'Savings'], ['spend', 'Spend']], spend ? 'spend' : 'savings') + '</div></div>' +
      legend(sum, list) + '<div class="tm-host">' + mapHTML(list, width, MAP_HEIGHT) + '</div></div>';
  }

  function share(savings, spend, approx) { return ui.pct(spend > 0 ? savings / spend : 0) + ' of ' + ui.money(spend, approx); }

  function bar(v, max) { return '<span class="rank-bar"><i style="width:' + (v > 0 ? Math.max(1, Math.round(v / max * 1000) / 10) : 0) + '%"></i></span>'; }

  function nsCard(sum, approx, all) {
    var list = sum.namespaces, shown = all ? list : list.slice(0, NS_ROWS);
    var max = list.length && list[0].savings > 0 ? list[0].savings : 1;
    var rows = shown.map(function (n) {
      var wl = ui.num(n.workloads, 0) + ' ' + ui.plural(n.workloads, 'workload');
      if (!n.counted) {
        return '<li><div class="rank rank-static rank-gap"><span class="rank-name"><span>' + esc(n.name) + '</span><span class="rank-sub">' + esc(wl) + ', not counted</span></span>' +
          '<span class="rank-bar rank-bar-gap"></span><span class="rank-value"><span>–</span><span class="rank-sub">' + esc(lowerFirst(n.reason)) + '</span></span></div></li>';
      }
      var left = n.workloads - n.counted;
      return '<li><div class="rank rank-static"><span class="rank-name"><span>' + esc(n.name) + '</span><span class="rank-sub">' + esc(wl + (left ? ', ' + left + ' not counted' : '')) + '</span></span>' +
        bar(n.savings, max) + '<span class="rank-value"><span>' + esc(ui.money(n.savings, approx)) + '</span><span class="rank-sub">' + esc(share(n.savings, n.spend, approx)) + '</span></span></div></li>';
    }).join('');
    var more = list.length > NS_ROWS;
    return '<div class="card"><h2>By namespace</h2><div class="sub">Savings and the share of each namespace’s spend' +
      (more && !all ? '. Showing ' + NS_ROWS + ' of ' + ui.num(list.length, 0) + '.' : '') + '</div>' +
      (rows ? '<ol class="ranks money-ranks">' + rows + '</ol>' : '<p class="empty">No namespaces in this view.</p>') +
      (more ? '<p class="money-more"><button type="button" class="link" data-all-ns>' + (all ? 'Show the top ' + NS_ROWS : 'Show all ' + ui.num(list.length, 0) + ' namespaces') + '</button></p>' : '') + '</div>';
  }

  function topCard(result, approx, filtered) {
    var counted = result.workloads.filter(function (w) { return w.series; });
    var top = counted.slice().sort(function (a, b) { return b.annualSavings - a.annualSavings || a.id.localeCompare(b.id); }).slice(0, TOP);
    var max = top.length && top[0].annualSavings > 0 ? top[0].annualSavings : 1;
    var rows = top.map(function (w) {
      return '<li><button type="button" class="rank" data-workload="' + esc(w.id) + '" aria-label="Show ' + esc(w.name) + ' in Workloads">' +
        '<span class="rank-name"><span>' + esc(w.name) + '</span><span class="rank-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</span></span>' + bar(w.annualSavings, max) +
        '<span class="rank-value"><span>' + esc(ui.money(w.annualSavings, approx)) + '</span><span class="rank-sub">' + esc(share(w.annualSavings, w.annualSpend, approx)) + '</span></span>' +
        '</button></li>';
    }).join('');
    var note = counted.length && counted.length < TOP ? '<p class="money-more muted">Only ' + counted.length + ' ' + ui.plural(counted.length, 'workload') +
      ' could be counted in this ' + (filtered ? 'view' : 'file') + ', so the list stops here.</p>' : '';
    return '<div class="card"><h2>Top ' + TOP + ' workloads</h2><div class="sub">Select one to see its history</div>' +
      (rows ? '<ol class="ranks money-ranks">' + rows + '</ol>' : '<p class="empty">No workload has complete data.</p>') + note + '</div>';
  }

  function lead(result) {
    return '<p class="money-lead">Annual savings by namespace and workload. ' + (result.workloads.length > MAP_WORKLOADS ?
      'With more than ' + MAP_WORKLOADS + ' workloads the map opens at the namespace level; select a namespace to see its workloads.' :
      'Cell size is savings; a hatched cell is a workload that could not be counted.') + '</p>';
  }

  // The whole section for a result, the shared state and the section's own
  // view: {level, metric, ns, allNs}. width is the map's width in pixels.
  function html(result, st, view, approx, width) {
    var sum = summarize(result), filtered = !!(st.namespaces || st.range);
    if (view.ns && !sum.tones[view.ns]) view = Object.assign({}, view, { ns: null });
    return lead(result) + mapCard(result, sum, view, approx, filtered, width) +
      '<div class="pair">' + nsCard(sum, approx, view.allNs) + topCard(result, approx, filtered) + '</div>';
  }

  var api = { summarize: summarize, squarify: squarify, cells: cells, html: html, MAP_WORKLOADS: MAP_WORKLOADS, MAP_CELLS: MAP_CELLS };
  if (node) { module.exports = api; return; }

  function freshView(data) { return { data: data, level: null, metric: 'savings', ns: null, allNs: false }; }

  function mapWidth(host) {
    var el = host.querySelector('.tm-host');
    return (el && el.clientWidth) || Math.max(240, host.clientWidth - 42) || 1100;
  }

  function draw(host) {
    var ctx = host.ctx, view = host.view;
    if (view.ns && !ctx.result.workloads.some(function (w) { return w.ns === view.ns; })) view.ns = null;
    host.innerHTML = html(ctx.result, ctx.state, view, ctx.approx, mapWidth(host));
  }

  function bind(host) {
    host.addEventListener('click', function (e) {
      var ctx = host.ctx, view = host.view, b = e.target.closest('button');
      if (!b) return;
      if (b.hasAttribute('data-workload')) { ctx.set({ workload: b.getAttribute('data-workload') }); ctx.go('workloads'); return; }
      if (b.hasAttribute('data-level')) { view.level = b.getAttribute('data-level'); view.ns = null; }
      else if (b.hasAttribute('data-metric')) view.metric = b.getAttribute('data-metric');
      else if (b.hasAttribute('data-ns')) view.ns = b.getAttribute('data-ns');
      else if (b.hasAttribute('data-map-back')) { view.ns = null; view.level = 'namespaces'; }
      else if (b.hasAttribute('data-all-ns')) view.allNs = !view.allNs;
      else return;
      draw(host);
    });
  }

  function render(host, ctx) {
    var first = !host.ctx;
    host.ctx = ctx;
    if (first) bind(host);
    if (!host.view || host.view.data !== ctx.data) host.view = freshView(ctx.data);
    // Choosing a workload or the engineer cost leaves this section as it was.
    else if (ctx.changed && !SP.state.affectsFigures(ctx.changed)) return;
    draw(host);
  }

  SP.sections.register({
    id: 'money', number: 2, title: 'Where the money is', render: render,
    resize: function (host) { if (host.ctx) draw(host); }
  });
})();
