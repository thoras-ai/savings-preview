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
    if (f.snapshot) return 'One reading, ' + time(f.start) + ' UTC';
    if (!st.range) return span(f.start, f.end);
    return span(Math.max(st.range[0], f.start), Math.min(st.range[1], f.end)) + ', the time range set under Assumptions. The file covers ' + span(f.start, f.end) + '.';
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
    return esc(r.pricing.basis) + ', set under Assumptions. The file’s own pricing: ' + esc(data.pricing.basis) + '.';
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
      ['Nodes', esc(nodesText(data.pricing))],
      ['Pricing', pricingText(r, data)]
    ];
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

  function who(w) { return '<span class="wl-name">' + esc(w.name) + '</span> <span class="mono-sub">' + esc(w.ns) + '</span>'; }

  // A workload with every container missing a required series, as opposed to
  // one left out for its replicas, its timing or the time range.
  function unread(w) { return !w.series && /^Missing /.test(w.reason) && w.reason !== 'Missing replica count'; }

  // One line per workload the totals leave out in whole or in part. Reasons
  // come from model.prepare and model.compute.
  function workloadItem(w) {
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

  function nodeItems(nodes) {
    var out = [];
    nodes.forEach(function (n) {
      var name = '<span class="wl-name">' + esc(n.name) + '</span>';
      if (!(n.cores > 0) || !(n.gib > 0)) out.push(name + ' has no allocatable CPU or memory, so it is not used for pricing.');
      else if (model.nodePrices[n.hostType] === undefined) out.push(name + (n.hostType && n.hostType !== 'unknown' ? ' has instance type ' + code(n.hostType) + ', which has no list price here,' : ' has no instance type,') + ' so it is not used for pricing.');
      else {
        if (n.capacity === 'spot') out.push(name + ' is a spot node priced at the on-demand list price.');
        if (n.region && n.region !== 'us-east-1') out.push(name + ' is in ' + esc(n.region) + ' and priced at us-east-1 list prices.');
      }
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
    var items = left.map(workloadItem).concat(nodeItems(data.nodes), r.fidelity.notes.map(esc));
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
      p.priced ? 'Price a core-hour and a GiB-hour from the node inventory: the priced nodes’ total list price divided by their total cores, and by their total GiB. Here that is ' + esc(rates(p)) + '.' :
        'Price a core-hour and a GiB-hour from the default node, ' + d.cores + ' cores, ' + d.gib + ' GiB, $' + d.hourly + ' per hour, since the file has no node with a known list price: ' + esc(rates(p)) + '.';
    var steps = [
      'Total each workload’s usage and requests across its replicas at every step, for CPU and for memory.',
      'Take the highest usage in the trailing ' + st.peakMinutes + '-minute window. The target is ' + code(target + ' × that peak') + '.' +
        (f.sourceStepSeconds ? ' Each point in this file keeps the highest ' + esc(model.formatStep(f.sourceStepSeconds)) + ' reading of its step, so short peaks are not lost.' : ''),
      'Waste at each step is ' + code('max(0, requested − target)') + '. A request below the target is left alone; the report never adds.',
      pricing,
      'A workload’s hourly cost is ' + code('max(CPU cost, memory cost)') + ': whichever resource runs out first decides how many nodes it needs. Current cost uses the request; optimized cost uses ' + code('min(request, target)') + '.',
      'Average over ' + (st.range ? 'the time range set under Assumptions' : 'the window') + ', weighting each point by its step, and multiply by 8,760 hours. Savings is current minus optimized. The cluster figure is the sum over counted workloads.'
    ];
    return '<div class="card"><h2>How the savings are computed</h2><div class="sub">Per workload, per step, for CPU and memory separately</div>' +
      '<ol class="method-steps">' + steps.map(function (s) { return '<li>' + s + '</li>'; }).join('') + '</ol></div>';
  }

  function notCard() {
    var items = [
      'Not a forecast. Thoras forecasts the next 15 minutes; this report uses the peak that actually happened, so it is what right-sizing would have done over the window.',
      'Not horizontal scaling. Replica counts are taken as they were. HPA and external metrics are out of scope.',
      'Not realized savings. Freed requests reduce the bill only when nodes are removed; see <a href="#section=cluster" data-go="cluster">Cluster and nodes</a>.',
      'Not your negotiated prices. Nodes are priced at on-demand list prices, spot nodes included; set your own rates under <a href="#section=assumptions" data-go="assumptions">Assumptions</a>.'
    ];
    return '<div class="card"><h2>What this is not</h2><ul class="facts-list">' + items.map(function (s) { return '<li>' + s + '</li>'; }).join('') + '</ul></div>';
  }

  function render(host, ctx) {
    var r = ctx.result, st = ctx.state;
    host.innerHTML = '<p class="section-lead">Where the numbers come from and how they are computed, so an engineer can check them.</p>' +
      '<div class="pair">' + dataCard(r, ctx.data, st) + missingCard(r, ctx.data) + '</div>' +
      methodCard(r, st) + notCard();
  }

  SP.sections.register({ id: 'method', number: 6, title: 'Data and method', render: render });
})();
