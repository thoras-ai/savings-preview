/* Section 6: data and method. Until it is built it lists the workloads left
 * out of the totals and why, which the summary links to. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc;

  function render(host, ctx) {
    var r = ctx.result;
    var left = r.workloads.filter(function (w) { return !w.series; });
    var partial = r.workloads.filter(function (w) { return w.status === 'partial'; });
    var item = function (w) {
      return '<li><span class="wl-name">' + esc(w.name) + '</span> <span class="wl-sub">' + esc(w.ns) + ' · ' + esc(w.kind) + '</span> ' + esc(w.reason) + '</li>';
    };
    host.innerHTML = '<div class="card"><h2>What is missing</h2><div class="sub">Workloads the totals leave out, in whole or in part</div>' +
      (left.length || partial.length ? '<ul class="missing">' + left.concat(partial).map(item).join('') + '</ul>' :
        '<p class="muted">Every workload has the data a savings figure needs.</p>') + '</div>';
  }

  SP.sections.register({ id: 'method', number: 6, title: 'Data and method', placeholder: true, render: render });
})();
