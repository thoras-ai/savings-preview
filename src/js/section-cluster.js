/* Section 4: cluster and nodes. What the nodes can hold, what the workloads
 * request, would need and use, the instance types behind the prices, and how
 * many nodes the waste adds up to. Plain script: registers the section in the
 * browser and exports summarize and markup under node. */
(function () {
  'use strict';

  var node = typeof module !== 'undefined' && module.exports;
  var model = node ? require('./model.js') : SP.model;
  var ui = node ? require('./components.js') : SP.ui;
  var esc = ui.esc;
  var CPU_ALLOC = 'k8s.node.cpu.allocatable', MEM_ALLOC = 'k8s.node.memory.allocatable';
  var CAPACITY = { on_demand: 'on-demand', spot: 'spot' };

  // Time-weighted mean of a node series over [from, to). A point holds for
  // its step, or until the next point in a held series. Time the node was
  // not there counts as zero, so summing nodes gives the cluster's capacity
  // averaged over the window.
  function levelOver(n, name, from, to) {
    var s = n.series[name];
    if (!s || !s.t.length || !(to > from)) return 0;
    var sum = 0, held = !!n.held[name];
    for (var i = 0; i < s.t.length; i++) {
      if (!isFinite(s.v[i])) continue;
      var next = i + 1 < s.t.length ? s.t[i + 1] : Infinity;
      var st = model.stepAt(n.spans, s.t[i], n.step);
      var end = held && next < Infinity ? next : Math.min(next, st > 0 ? s.t[i] + st : next);
      var a = Math.max(s.t[i], from), b = Math.min(end, to);
      if (b > a) sum += s.v[i] * (b - a);
    }
    return sum / (to - from);
  }

  // The window the figures cover: the time range when one is set, else the file.
  function windowOf(r, st) {
    var f = r.fidelity;
    if (f.start === null) return null;
    if (!st.range) return [f.start, f.end];
    var a = Math.max(st.range[0], f.start), b = Math.min(st.range[1], f.end);
    return b > a ? [a, b] : null;
  }

  function mix(nodes) {
    var groups = {}, order = [];
    nodes.forEach(function (n) {
      var type = n.hostType === 'unknown' ? '' : n.hostType, key = type + '|' + n.capacity;
      if (!groups[key]) {
        groups[key] = { type: type, capacity: CAPACITY[n.capacity] || '', count: 0, cores: 0, gib: 0, hourly: model.nodePrices[n.hostType] };
        order.push(key);
      }
      var g = groups[key];
      g.count++; g.cores += n.cores; g.gib += n.gib;
    });
    return order.map(function (k) {
      var g = groups[k];
      g.cores /= g.count; g.gib /= g.count;
      return g;
    }).sort(function (a, b) {
      return b.count - a.count || (a.hourly === undefined ? Infinity : a.hourly) - (b.hourly === undefined ? Infinity : b.hourly) ||
        a.type.localeCompare(b.type) || a.capacity.localeCompare(b.capacity);
    });
  }

  // The section's figures under the current levers. Requested, waste and
  // used come from the model's totals; target is what the workloads would
  // request once right-sized, so requested minus target is the waste.
  function summarize(data, r, st) {
    var t = r.totals, nodes = data.nodes;
    var win = windowOf(r, st), alloc = null;
    if (nodes.length && win) {
      alloc = { cores: 0, gib: 0 };
      nodes.forEach(function (n) {
        alloc.cores += levelOver(n, CPU_ALLOC, win[0], win[1]);
        alloc.gib += levelOver(n, MEM_ALLOC, win[0], win[1]) / model.constants.GIB;
      });
      if (!(alloc.cores > 0 && alloc.gib > 0)) alloc = null;
    }
    var sized = nodes.filter(function (n) { return n.cores > 0 && n.gib > 0; });
    var avg = sized.length ? {
      cores: sized.reduce(function (a, n) { return a + n.cores; }, 0) / sized.length,
      gib: sized.reduce(function (a, n) { return a + n.gib; }, 0) / sized.length,
      fromNodes: true
    } : { cores: model.constants.DEFAULT_NODE.cores, gib: model.constants.DEFAULT_NODE.gib, fromNodes: false };
    var cpuNodes = t.cores / avg.cores, memNodes = t.gib / avg.gib, freed = Math.min(cpuNodes, memNodes);
    var types = {};
    nodes.forEach(function (n) { types[n.hostType === 'unknown' ? '' : n.hostType] = true; });
    var counted = t.counted > 0;
    return {
      nodes: nodes.length, types: Object.keys(types).length, priced: data.pricing.priced,
      alloc: alloc,
      cpu: { req: counted ? t.reqCores : NaN, target: counted ? t.reqCores - t.cores : NaN, used: counted ? t.useCores : NaN, waste: t.cores },
      mem: { req: counted ? t.reqGiB : NaN, target: counted ? t.reqGiB - t.gib : NaN, used: counted ? t.useGiB : NaN, waste: t.gib },
      mix: mix(nodes),
      freed: { nodes: freed, cpu: cpuNodes, mem: memNodes, whole: Math.min(Math.floor(freed + 1e-9), nodes.length || Infinity), avg: avg },
      left: t.workloads - t.counted,
      filtered: !!st.namespaces, ranged: !!st.range,
      spot: nodes.some(function (n) { return n.capacity === 'spot' && model.nodePrices[n.hostType] !== undefined; }),
      regions: nodes.some(function (n) { return n.region && n.region !== 'us-east-1' && model.nodePrices[n.hostType] !== undefined; }),
      customRates: !!r.pricing.custom
    };
  }

  // approx rounds the figure to two significant figures and marks it with ~.
  function amount(v, approx) {
    if (!isFinite(v)) return '–';
    var x = approx ? ui.rough(v) : v;
    return (approx ? '~' : '') + ui.num(x, Math.abs(x) < 1 ? 2 : 1);
  }

  // Node equivalents round down, so 38.99 does not read as 39 next to 38 whole nodes.
  function nodeFigure(v, approx) {
    var p = Math.abs(v) < 1 ? 100 : 10;
    return amount(approx ? v : Math.floor(v * p + 1e-9) / p, approx);
  }

  // A tile too narrow for both figures breaks the line between them.
  function pair(cores, gib, approx) {
    if (!isFinite(cores)) return '–';
    return amount(cores, approx) + '\u00a0cores · ' + amount(gib, approx) + '\u00a0GiB';
  }

  function share(s, key) {
    if (!s.alloc || !isFinite(s.cpu[key])) return '';
    return ui.pct(s.cpu[key] / s.alloc.cores) + ' of CPU, ' + ui.pct(s.mem[key] / s.alloc.gib) + ' of memory';
  }

  function tiles(s, approx) {
    var edge = approx ? 'provisional' : 'default';
    var priced = s.priced === s.nodes ? (s.nodes === 1 ? 'priced' : 'all priced') : s.priced ? s.priced + ' priced' : 'none priced';
    var nodesContext = s.nodes ? s.types + ' instance ' + ui.plural(s.types, 'type') + ', ' + priced : 'The file lists no nodes';
    var over = s.ranged ? 'Across all nodes, averaged over the time range' : 'Across all nodes, averaged over the window';
    return '<div class="kpis">' +
      ui.kpiTile({ label: 'Nodes in the file', value: String(s.nodes), context: nodesContext }) +
      ui.kpiTile({ label: 'Allocatable', value: s.alloc ? pair(s.alloc.cores, s.alloc.gib, false) : '–', context: s.alloc ? over : 'No node capacity in the file' }) +
      ui.kpiTile({ label: 'Requested by workloads', value: pair(s.cpu.req, s.mem.req, approx), context: share(s, 'req') || 'Averaged over the window', variant: edge }) +
      ui.kpiTile({ label: 'Used by workloads', value: pair(s.cpu.used, s.mem.used, approx), context: share(s, 'used') || 'Mean usage over the window', variant: edge }) +
      '</div>';
  }

  function width(v, scale) { return (scale > 0 && v > 0 ? Math.min(100, v / scale * 100) : 0).toFixed(1) + '%'; }

  // Used over target over requested, against allocatable capacity, or
  // against requests when the file has no nodes.
  function bar(label, r, cap, unit, approx) {
    var scale = cap || r.req;
    var figures = amount(r.used, approx) + ' used · ' + amount(r.target, approx) + ' target · ' + amount(r.req, approx) + ' req' +
      (cap ? ' · ' + amount(cap, false) + ' ' + unit : ' ' + unit);
    var aria = label + ': ' + figures.replace(/ · /g, ', ').replace(' req', ' requested') + (cap ? ' allocatable' : '');
    return '<div class="cap-label">' + label + '</div>' +
      '<div class="cap-bar" role="img" aria-label="' + esc(aria) + '">' +
      '<i class="cap-req" style="width:' + width(r.req, scale) + '"></i>' +
      '<i class="cap-target" style="width:' + width(r.target, scale) + '"></i>' +
      '<i class="cap-used" style="width:' + width(r.used, scale) + '"></i></div>' +
      '<div class="cap-figures">' + esc(figures) + '</div>';
  }

  function capacityCard(s, approx) {
    var a = s.alloc, notes = [];
    var sub = a ? 'Across the nodes in the file, averaged over the ' + (s.ranged ? 'time range' : 'window') + '. The bar is allocatable capacity.' :
      'The file has no node capacity, so the bar is what the workloads request.';
    if (s.filtered) sub += ' Workloads in the selected namespaces only.';
    var over = a && [s.cpu.req > a.cores ? 'CPU' : '', s.mem.req > a.gib ? 'memory' : ''].filter(Boolean);
    if (over && over.length) {
      notes.push('Workloads request more ' + over.join(' and ') + ' than the nodes in the file can allocate. Requests count every desired replica, ' +
        'including pods that could not be scheduled, and the file may not list every node the workloads ran on.');
    }
    if (s.left) {
      notes.push(s.left + ' ' + ui.plural(s.left, 'workload') + ' with too little data ' + (s.left === 1 ? 'is' : 'are') +
        ' not in the requested and used figures, so the cluster reserves more than shown.');
    }
    var body = isFinite(s.cpu.req) ?
      '<div class="legend cap-legend"><span><i class="cap-swatch cap-used"></i>Used</span><span><i class="cap-swatch cap-target"></i>Target (usage + headroom)</span>' +
      '<span><i class="cap-swatch cap-req"></i>Requested</span></div>' +
      '<div class="cap-grid">' + bar('CPU', s.cpu, a && a.cores, 'cores', approx) + bar('Memory', s.mem, a && a.gib, 'GiB', approx) + '</div>' :
      '<p class="empty">No workload has complete data, so there is nothing to compare with the nodes.</p>';
    return '<div class="card"><h2>Allocatable, requested and used</h2><div class="sub">' + esc(sub) + '</div>' + body +
      notes.map(function (n) { return '<p class="cl-note">' + esc(n) + '</p>'; }).join('') + '</div>';
  }

  function mixCard(s) {
    var notes = [];
    if (!s.nodes) {
      notes.push('The file lists no nodes. Requests are priced with the default node (4 cores, 16 GiB, $0.192 per hour).');
    } else {
      if (!s.priced) notes.push('No instance type in the file has a known list price, so requests are priced with the default node (4 cores, 16 GiB, $0.192 per hour).');
      else if (s.priced < s.nodes) notes.push('Nodes without a known list price are left out of the per-core and per-GiB rates.');
      if (s.spot) notes.push('Spot nodes are priced at the on-demand list price.');
      if (s.regions) notes.push('Prices are AWS us-east-1 list prices; nodes in other regions are priced as if they were in it.');
    }
    if (s.customRates) notes.push('The figures in this report use the rates set under Assumptions, not these prices.');
    var rows = s.mix.map(function (g) {
      return '<tr><td>' + (g.type ? '<code>' + esc(g.type) + '</code>' : '<span class="muted">Not stated</span>') + '</td>' +
        '<td class="num">' + g.count + '</td><td>' + (g.capacity ? esc(g.capacity) : '<span class="muted">Not stated</span>') + '</td>' +
        '<td class="num">' + ui.num(g.cores, 2) + '</td><td class="num">' + ui.num(g.gib, 1) + '</td>' +
        '<td class="num">' + (g.hourly !== undefined ? '$' + g.hourly.toFixed(g.hourly < 0.1 ? 4 : 3) : '<span class="muted">Not priced</span>') + '</td></tr>';
    }).join('');
    var table = rows ? '<div class="table-wrap"><table class="cl-mix"><thead><tr><th scope="col">Instance type</th><th scope="col" class="num">Nodes</th>' +
      '<th scope="col">Capacity</th><th scope="col" class="num">Cores</th><th scope="col" class="num">GiB</th><th scope="col" class="num">$/hour</th></tr></thead>' +
      '<tbody>' + rows + '</tbody></table></div>' : '';
    return '<div class="card"><h2>Node mix</h2><div class="sub">Instance types in the file and their list prices</div>' + table +
      notes.map(function (n) { return '<p class="cl-note">' + esc(n) + '</p>'; }).join('') + '</div>';
  }

  function freedCard(s, approx) {
    var f = s.freed, avg = f.avg;
    var size = '(' + ui.num(avg.cores, 1) + ' cores, ' + ui.num(avg.gib, 1) + ' GiB)';
    var of = avg.fromNodes ? 'of ' + s.nodes + ', as capacity freed at the average node ' + size :
      'as capacity freed at the default node ' + size + ', because the file lists no nodes';
    var whole = f.whole >= 1 ?
      fig(f.whole + ' whole ' + ui.plural(f.whole, 'node'), 'could be removed at the average size, if the cluster autoscaler packs the remaining requests onto fewer nodes.') :
      fig('Less than one node', 'Not enough freed capacity to remove a whole node of the average size, so right-sizing alone would not lower the node bill.');
    return '<div class="card"><h2>Nodes that could be freed</h2><div class="sub">' + (s.filtered ? 'Waste in the selected namespaces' : 'Waste') + ' expressed as node equivalents</div>' +
      '<div class="cl-freed">' + fig(nodeFigure(f.nodes, approx) + ' ' + ui.plural(f.nodes === 1 ? 1 : 2, 'node'), esc(of)) + whole + '</div>' +
      '<p class="cl-note">Freed requests lower the bill only when the cluster autoscaler removes nodes. CPU waste alone would free ' +
      esc(nodeFigure(f.cpu, approx)) + ' ' + ui.plural(f.cpu === 1 ? 1 : 2, 'node') + ' and memory waste ' + esc(nodeFigure(f.mem, approx)) +
      '; the smaller number is what the cluster can give up. The savings figure elsewhere in this report is based on requests.</p></div>';
  }

  function fig(big, small) {
    return '<div class="cl-fig"><div class="meaning-figure">' + esc(big) + '</div><div class="muted">' + small + '</div></div>';
  }

  function markup(s, approx) {
    return '<p class="cl-lead">What the cluster has, what workloads reserve, and what they use.</p>' +
      tiles(s, approx) + capacityCard(s, approx) +
      '<div class="pair cl-pair">' + mixCard(s) + freedCard(s, approx) + '</div>';
  }

  function render(host, ctx) {
    host.classList.toggle('provisional', ctx.approx);
    host.innerHTML = markup(summarize(ctx.data, ctx.result, ctx.state), ctx.approx);
  }

  if (node) module.exports = { summarize: summarize, markup: markup, levelOver: levelOver };
  else SP.sections.register({ id: 'cluster', number: 4, title: 'Cluster and nodes', render: render });
})();
