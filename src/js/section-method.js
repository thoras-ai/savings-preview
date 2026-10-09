/* Section 6: data and method. What the file holds, what it lacks, and how the
 * figures are computed under the current settings, so an engineer can check
 * them. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc, model = SP.model;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  // Longer lists fold the rest behind a toggle, so 1,000 workloads stay readable.
  var SHOWN = 8;

  var TIERS = {
    1: 'at least 14 days at 1-minute resolution.',
    2: 'at least 7 days at 5-minute resolution.',
    3: 'less history or coarser samples than tier 2.'
  };

  var REPLICAS = { deployment: 'k8s.deployment.pod.desired', statefulset: 'k8s.statefulset.pod.desired' };

  var METRIC_NAMES = {
    'container.cpu.usage': 'CPU usage',
    'container.memory.working_set': 'memory usage',
    'k8s.container.cpu.request.desired': 'CPU request',
    'k8s.container.memory.request.desired': 'memory request'
  };

  function code(s) { return '<code>' + esc(s) + '</code>'; }

  function time(sec) {
    var d = new Date(sec * 1000), h = d.getUTCHours(), m = d.getUTCMinutes();
    return MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate() + ', ' + ((h + 11) % 12 + 1) + ':' + (m < 10 ? '0' : '') + m + ' ' + (h < 12 ? 'AM' : 'PM');
  }

  function span(a, b) { return time(a) + ' to ' + time(b) + ' UTC, ' + model.formatSpan(b - a); }

  function windowText(r, st) {
    var f = r.fidelity;
    if (f.start === null) return 'No usage readings';
    var all = f.snapshot ? 'one reading, ' + time(f.start) + ' UTC' : span(f.start, f.end);
    var a = st.range ? Math.max(st.range[0], f.start) : f.start, b = st.range ? Math.min(st.range[1], f.end) : f.end;
    if (st.range && a >= b) return 'No data in the time range set under Assumptions. The file covers ' + all + '.';
    if (f.snapshot) return 'One reading, ' + time(f.start) + ' UTC';
    if (!st.range) return all;
    return span(a, b) + ', the time range set under Assumptions. The file covers ' + all + '.';
  }

  function resolution(f) {
    if (f.snapshot) return 'A single snapshot';
    if (f.sourceStepSeconds) return model.formatStep(f.sourceStepSeconds) + ' samples, kept as ' + model.formatStep(f.stepSeconds) + ' peaks and means';
    return model.formatStep(f.stepSeconds) + ' samples';
  }

  function fidelityText(f) {
    return ui.confidencePill(ui.confidenceLevel(f)) + ' tier ' + f.tier + ': ' + esc(f.snapshot ? 'a single snapshot, with no history.' : TIERS[f.tier] || TIERS[3]);
  }

  function workloadsText(r, data, st) {
    var t = r.totals, all = ui.num(data.workloads.length);
    if (!st.namespaces) return ui.num(t.counted) + ' counted of ' + all + ' in the file';
    return ui.num(t.counted) + ' counted of ' + ui.num(t.workloads) + ' in ' + (st.namespaces.length === 1 ? 'the namespace ' + st.namespaces[0] : st.namespaces.length + ' selected namespaces') +
      ', ' + all + ' in the file';
  }

  function nodesText(p) {
    if (!p.nodes) return 'None in the file';
    return ui.num(p.nodes) + ' in the file, ' + ui.num(p.priced) + ' with a known list price';
  }

  function pricingText(r, data) {
    if (!r.pricing.custom) return esc(r.pricing.basis);
    return esc(r.pricing.basis) + ', set under Assumptions. The file’s own pricing: ' + esc((r.basePricing || data.pricing).basis) + '.';
  }

  function clustersText(r, data) {
    var n = data.clusters.length, shown = r.clusters.length;
    var text = shown < n ? ui.num(shown) + ' of ' + ui.num(n) + ' in the file, as set under Assumptions' : ui.num(n) + ' in the file';
    if (data.failed.length) text += '; ' + ui.num(data.failed.length) + ' not read';
    return text;
  }

  function contractText(c) {
    if (!c || !c.version) return '';
    var semconv = /\/schemas\/([^/]+)$/.exec(c.schemaUrl || '');
    return 'thoras.savings_preview ' + c.version + (semconv ? ', OpenTelemetry semantic conventions ' + semconv[1] : '');
  }

  function dataCard(r, data, st) {
    var rows = [
      ['Cluster', esc(r.cluster || 'Unnamed cluster')],
      ['Source', esc(r.fidelity.source || 'Not recorded')],
      ['Window', esc(windowText(r, st))],
      ['Resolution', esc(resolution(r.fidelity))],
      ['Fidelity', fidelityText(r.fidelity)],
      ['Workloads', esc(workloadsText(r, data, st))],
      ['Nodes', esc(nodesText(r.basePricing || data.pricing))],
      ['Pricing', pricingText(r, data)]
    ];
    if (data.multi) rows[0] = ['Clusters', esc(clustersText(r, data))];
    else if (data.failed && data.failed.length) rows.splice(1, 0, ['Clusters', esc(clustersText(r, data))]);
    var contract = contractText(data.contract);
    if (contract) rows.push(['Contract', esc(contract)]);
    return '<div class="card"><h2>The data</h2><dl class="facts">' +
      rows.map(function (row) { return '<dt>' + row[0] + '</dt><dd>' + row[1] + '</dd>'; }).join('') + '</dl></div>';
  }

  function metrics(list) {
    return list.map(function (m) { return esc(METRIC_NAMES[m] || m) + ' (' + code(m) + ')'; }).join(' and ');
  }

  function missingOf(w) {
    var out = [];
    w.containers.forEach(function (c) { c.missing.forEach(function (m) { if (out.indexOf(m) < 0) out.push(m); }); });
    return out;
  }

  // labels, in a file with several clusters, names each workload's cluster.
  function whoIs(w, labels) {
    var where = labels ? labels[w.cluster] + ' · ' + w.nsName : w.ns;
    return '<span class="wl-name">' + esc(w.name) + '</span> <span class="mono-sub"' + (labels ? ' title="' + esc(w.cluster) + '"' : '') + '>' + esc(where) + '</span>';
  }

  // A workload with every container missing a required series, as opposed to
  // one left out for its replicas, its timing or the time range.
  function unread(w) { return !w.series && /^Missing /.test(w.reason) && w.reason !== 'Missing replica count'; }

  // One line per workload the totals leave out in whole or in part. Reasons
  // come from model.prepare and model.compute.
  function workloadItem(w, labels) {
    var who = function (x) { return whoIs(x, labels); };
    if (w.reason === 'No data in the selected time range') return who(w) + ': no data in the time range set under Assumptions, so it is not counted.';
    if (w.reason === 'Missing replica count') return who(w) + ': no replica count (' + code(REPLICAS[w.kind]) + '), so it is listed but not counted.';
    if (w.reason === 'No time with complete data') return who(w) + ': its series never all have a value at the same time, so it is listed but not counted.';
    if (unread(w)) return who(w) + ': missing ' + metrics(missingOf(w)) + ' data, so it is listed but not counted.';
    if (w.status === 'partial') {
      var parts = w.containers.filter(function (c) { return c.excluded; });
      return who(w) + ': container ' + parts.map(function (c) { return esc(c.name) + ' is missing ' + metrics(c.missing); }).join('; container ') +
        ', so ' + (parts.length === 1 ? 'that container is' : 'those containers are') + ' left out of its figures.';
    }
    return who(w) + ': ' + esc(w.reason) + '.';
  }

  // With rates set under Assumptions the nodes do not set prices, so only the
  // nodes the file could not price are worth naming.
  function nodeItems(nodes, custom) {
    var out = [], inferred = {};
    nodes.forEach(function (n) {
      var name = '<span class="wl-name">' + esc(n.name) + '</span>', p = model.priceNode(n), type = code(n.hostType);
      if (p.reason === 'type') out.push(name + (n.hostType && n.hostType !== 'unknown' ? ' has instance type ' + type + ', which has no list price here,' : ' has no instance type,') + ' so it is not used for pricing.');
      else if (p.reason === 'cloud') out.push(name + ' runs on ' + esc(n.provider) + ', which has no list prices here, so it is not used for pricing.');
      else if (p.reason === 'size') out.push(name + ' has custom type ' + type + ' with no size in its name and no allocatable CPU or memory, so it is not used for pricing.');
      else if (p.reason === 'spot') out.push(name + ' is a spot node of type ' + type + ', which has no spot list price, so it is not used for pricing.');
      else if (!custom) {
        var cloud = model.cloudNames[p.cloud];
        if (p.inferred) inferred[p.cloud] = (inferred[p.cloud] || 0) + 1;
        if (p.custom) out.push(name + ' has custom type ' + type + ', priced per vCPU and per GiB for ' + ui.num(p.cores, 2) + ' vCPU and ' + ui.num(p.gib, 2) + ' GiB.');
        if (p.azureSpot) out.push(name + ' is an Azure spot node priced at the on-demand list price.');
        if (p.otherRegion) out.push(name + ' is in ' + esc(n.region) + ' and priced at ' + cloud + ' ' + esc(model.priceTable[p.cloud].region) + ' list prices.');
      }
    });
    Object.keys(inferred).forEach(function (c) {
      var k = inferred[c];
      out.push(ui.num(k) + ' ' + ui.plural(k, 'node') + ' with no cloud provider in the file ' + (k === 1 ? 'is' : 'are') + ' priced as ' + model.cloudNames[c] + ', the cloud ' + (k === 1 ? 'its instance type belongs' : 'their instance types belong') + ' to.');
    });
    return out;
  }

  function list(items) {
    var li = function (s) { return '<li>' + s + '</li>'; };
    var html = '<ul class="facts-list">' + items.slice(0, SHOWN).map(li).join('') + '</ul>';
    if (items.length > SHOWN) {
      html += '<details class="more"><summary>Show ' + (items.length - SHOWN) + ' more</summary><ul class="facts-list">' +
        items.slice(SHOWN).map(li).join('') + '</ul></details>';
    }
    return html;
  }

  function missingCard(r, data) {
    var left = r.workloads.filter(function (w) { return !w.series || w.status === 'partial'; });
    var labels = data.multi ? data.labels : null;
    var items = left.map(function (w) { return workloadItem(w, labels); }).concat(nodeItems(r.nodes || data.nodes, r.pricing.custom), r.fidelity.notes.map(esc));
    var after = [];
    if (left.some(unread)) after.push('A workload’s cost is the larger of its CPU and memory cost, so it cannot be computed from one of them.');
    after.push((left.length ? 'Every other' : 'Every') + ' required series was present for every workload.');
    if (!r.fidelity.notes.length) after.push('No fidelity notes were recorded.');
    return '<div class="card"><h2>Missing or derived</h2><div class="sub">What the file says it could not get, and how prices were found</div>' +
      (items.length ? list(items) : '') + '<p class="muted method-after">' + after.join(' ') + '</p></div>';
  }

  function rates(p) {
    return '$' + Number(p.perCore.toPrecision(3)) + ' per core-hour and $' + Number(p.perGiB.toPrecision(3)) + ' per GiB-hour';
  }

  function methodCard(r, st) {
    var f = r.fidelity, p = r.pricing, d = model.constants.DEFAULT_NODE;
    var target = ui.num(1 + st.headroom, 2);
    var pricing = p.custom ? 'Price a core-hour and a GiB-hour at the rates set under Assumptions: ' + esc(rates(p)) + '.' :
      r.clusters && r.clusters.length > 1 ? 'Price a core-hour and a GiB-hour for each cluster from its own nodes: the priced nodes’ total list price divided by their total listed vCPUs, and by their total listed GiB, or the default node, ' + d.cores + ' cores, ' + d.gib + ' GiB, $' + d.hourly + ' per hour, for a cluster with no node of a known list price. Spot nodes count at their spot list price.' :
      p.priced ? 'Price a core-hour and a GiB-hour from the node inventory: the priced nodes’ total list price divided by their total listed vCPUs, and by their total listed GiB. Spot nodes count at their spot list price. Here that is ' + esc(rates(p)) + '.' :
        'Price a core-hour and a GiB-hour from the default node, ' + d.cores + ' cores, ' + d.gib + ' GiB, $' + d.hourly + ' per hour, since the file has no node with a known list price: ' + esc(rates(p)) + '.';
    var steps = [
      'Total each workload’s usage and requests across its replicas at every step, for CPU and for memory. A container counts at a step only when it has CPU and memory usage there; one without them, such as an init container or a check that runs now and then, is not running and adds no usage and no request.',
      'Take the highest usage in the trailing ' + st.peakMinutes + '-minute window. The target is ' + code(target + ' × that peak') + '.' +
        (f.sourceStepSeconds ? ' Each point in this file keeps the highest ' + esc(model.formatStep(f.sourceStepSeconds)) + ' reading of its step, so short peaks are not lost.' : ''),
      'Waste at each step is ' + code('max(0, requested − target)') + '. A request below the target is left alone; the report never adds.',
      pricing,
      'A workload’s hourly cost is ' + code('max(CPU cost, memory cost)') + ': whichever resource runs out first decides how many nodes it needs. Current cost uses the request; optimized cost uses ' + code('min(request, target)') + '.',
      'Average over ' + (st.range ? 'the time range set under Assumptions' : 'the window') + ', weighting each point by its step, and multiply by 8,760 hours. Savings is current minus optimized. The cluster figure is the sum over counted workloads' + (r.fleet ? ', and the fleet figure the sum over its clusters.' : '.')
    ];
    return '<div class="card"><h2>How the savings are computed</h2><div class="sub">Per workload, per step, for CPU and memory separately</div>' +
      '<ol class="method-steps">' + steps.map(function (s) { return '<li>' + s + '</li>'; }).join('') + '</ol></div>';
  }

  // A cluster's short name, with its full name on hover and under it when the two differ.
  function clusterName(data, name) {
    var label = (data.labels && data.labels[name]) || name;
    return '<span class="wl-name" title="' + esc(name) + '">' + esc(label) + '</span>' + (label !== name ? '<div class="mono-sub">' + esc(name) + '</div>' : '');
  }

  // Each cluster in the file: what was read and what it comes to, the
  // clusters that could not be read and why, and the clusters that came from
  // more than one file.
  function fleetCard(r, data) {
    var figures = {};
    r.clusters.forEach(function (c) { figures[c.name] = c; });
    var rows = data.clusters.map(function (c) {
      var f = c.fidelity, x = figures[c.name];
      return '<tr><td>' + clusterName(data, c.name) + '</td><td>' + esc(f.source || 'Not recorded') + '</td>' +
        '<td>' + esc(f.label) + '</td><td class="num">' + f.tier + '</td>' +
        '<td class="num">' + (x ? esc(ui.num(x.counted) + ' of ' + ui.num(x.workloads)) : 'excluded') + '</td>' +
        '<td class="num">' + (x ? esc(ui.money(x.spend)) : '') + '</td><td class="num">' + (x ? esc(ui.money(x.savings)) : '') + '</td></tr>';
    }).concat(data.failed.map(function (f) {
      return '<tr><td>' + clusterName(data, f.name) + '</td><td colspan="6">Not read: ' + esc(f.reason) + '</td></tr>';
    })).join('');
    var notes = data.dropped.map(function (d) {
      return 'Cluster <span class="wl-name" title="' + esc(d.cluster) + '">' + esc((data.labels && data.labels[d.cluster]) || d.cluster) + '</span> is in ' + esc(d.kept) + ' and ' + esc(d.dropped) +
        '. The figures use ' + esc(d.kept) + ', whose data is newer, and leave out ' + esc(d.dropped) + ', so the cluster is counted once.';
    });
    if (data.failed.length) notes.push('A cluster that could not be read adds nothing to the figures. Run it again once the reason above is fixed, and open both files together.');
    return '<div class="card"><h2>Clusters</h2><div class="sub">Each cluster is priced from its own nodes; the fleet figures are their sum</div>' +
      '<div class="table-wrap"><table class="sum-table"><thead><tr><th scope="col">Cluster</th><th scope="col">Source</th><th scope="col">Data</th>' +
      '<th scope="col" class="num">Tier</th><th scope="col" class="num">Workloads counted</th><th scope="col" class="num">Spend / yr</th><th scope="col" class="num">Saves / yr</th></tr></thead>' +
      '<tbody>' + rows + '</tbody></table></div>' + (notes.length ? list(notes) : '') + '</div>';
  }

  function notCard() {
    var items = [
      'Not a forecast. Thoras forecasts the next 15 minutes; this report uses the peak that actually happened, so it is what right-sizing would have done over the window.',
      'Not horizontal scaling. Replica counts are taken as they were. HPA and external metrics are out of scope.',
      'Not realized savings. Freed requests reduce the bill only when nodes are removed; see <a href="#section=cluster" data-go="cluster">Cluster and nodes</a>.',
      'Not your negotiated prices. Nodes are priced at list prices, on-demand or spot; set your own rates under <a href="#section=assumptions" data-go="assumptions">Assumptions</a>.'
    ];
    return '<div class="card"><h2>What this is not</h2><ul class="facts-list">' + items.map(function (s) { return '<li>' + s + '</li>'; }).join('') + '</ul></div>';
  }

  function render(host, ctx) {
    var r = ctx.result, st = ctx.state;
    host.innerHTML = '<p class="section-lead">Where the numbers come from and how they are computed, so an engineer can check them.</p>' +
      '<div class="pair">' + dataCard(r, ctx.data, st) + missingCard(r, ctx.data) + '</div>' +
      (ctx.data.multi || ctx.data.failed.length || ctx.data.dropped.length ? fleetCard(r, ctx.data) : '') +
      methodCard(r, st) + notCard();
  }

  SP.sections.register({ id: 'method', number: 6, title: 'Data and method', render: render });
})();
