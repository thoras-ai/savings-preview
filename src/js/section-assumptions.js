/* Section 5: assumptions and levers. Until the lever controls are built it
 * lists the settings in effect, which links can change through the hash. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc;

  function rows(ctx) {
    var st = ctx.state, r = ctx.result;
    var range = st.range ? new Date(st.range[0] * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' to ' +
      new Date(st.range[1] * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'The whole file';
    return [
      ['Pricing', r.pricing.basis],
      ['Headroom', ui.num(st.headroom * 100, 1) + '% over the peak'],
      ['Peak window', st.peakMinutes + ' minutes'],
      ['Namespaces', st.namespaces ? st.namespaces.join(', ') : 'All ' + r.namespaces.length],
      ['Time range', range],
      ['Engineer loaded cost', ui.money(st.engineerCost) + ' a year']
    ];
  }

  function render(host, ctx) {
    var custom = SP.state.serialize(Object.assign({}, ctx.state, { section: SP.state.DEFAULTS.section, workload: null })) !== '';
    host.innerHTML = '<div class="card"><h2>Settings in effect</h2><div class="sub">Every section is computed with these.</div>' +
      '<dl class="settings">' + rows(ctx).map(function (r) { return '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>'; }).join('') + '</dl>' +
      (custom ? '<button type="button" class="link settings-reset">Reset to defaults</button>' : '') + '</div>';
    var reset = host.querySelector('.settings-reset');
    if (reset) reset.addEventListener('click', function () {
      var d = SP.state.DEFAULTS;
      ctx.set({ rates: d.rates, headroom: d.headroom, peakMinutes: d.peakMinutes, namespaces: d.namespaces, range: d.range, engineerCost: d.engineerCost });
    });
  }

  SP.sections.register({ id: 'assumptions', number: 5, title: 'Assumptions', placeholder: true, render: render });
})();
