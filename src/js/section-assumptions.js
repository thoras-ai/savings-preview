/* Section 5: assumptions and levers. Every control writes the shared state,
 * which recomputes every section and keeps the settings in the link. The
 * controls are built once per file and updated in place, so a control keeps
 * focus while the page recomputes. Sliders recompute when released. */
(function () {
  'use strict';

  var ui = SP.ui, esc = ui.esc, levers = SP.levers, prices = SP.prices, DEFAULTS = SP.state.DEFAULTS;

  // Built once per file: the controls, and the cloud and region the type list shows.
  var view = null;

  function q(host, sel) { return host.querySelector(sel); }

  function reset(name) {
    return '<button type="button" class="link lv-reset" data-reset="' + name + '" hidden>Reset</button>';
  }

  // opts.value shows the slider's setting next to the label; opts.reset names
  // the lever its reset button resets, or false for none.
  function field(label, name, control, opts) {
    opts = opts || {};
    var lever = opts.reset === undefined ? name : opts.reset;
    return '<div class="lv-field' + (opts.wide ? ' lv-wide' : '') + '"><div class="lv-label"><label for="lv-' + name + '">' + esc(label) + '</label>' +
      (opts.value ? ' <span class="lv-value" data-value="' + name + '"></span>' : '') + (lever ? reset(lever) : '') + '</div>' + control + '</div>';
  }

  var EXAMPLE = { aws: 'm5.xlarge', gcp: 'n2-standard-4', azure: 'Standard_D4s_v5' };

  function options(list) {
    return list.map(function (o) { return '<option value="' + esc(o.value) + '">' + esc(o.label) + '</option>'; }).join('');
  }

  function cloudOptions() {
    return Object.keys(prices.CLOUDS).map(function (c) { return { value: c, label: prices.CLOUDS[c].name }; });
  }

  function regionOptions(cloud) {
    return Object.keys(prices.CLOUDS[cloud].regions).map(function (r) { return { value: r, label: r }; });
  }

  function typeList(cloud, region) {
    return prices.types(cloud).map(function (t) {
      var p = prices.lookup(cloud, region, t);
      return '<option value="' + esc(t) + '" label="' + esc(p.cores + ' vCPU, ' + ui.num(p.gib, 2) + ' GiB, $' + p.hourly.toFixed(4) + ' per hour') + '"></option>';
    }).join('');
  }

  function rightSizing(data) {
    var windows = levers.peakWindows(data.fidelity.stepSeconds, data.fidelity.snapshot);
    var ticks = windows.map(function (w) {
      return '<span class="lv-tick' + (w.disabled ? ' off' : '') + '"' + (w.disabled ? ' title="Finer than the data can show"' : '') + '>' + label(w.minutes) + '</span>';
    }).join('');
    return '<div class="card"><h2>Right-sizing</h2><div class="sub">How the target is built</div>' +
      '<div class="lv-grid">' +
      field('Headroom above peak', 'headroom', '<input id="lv-headroom" type="range" min="0" max="50" step="5" data-lever="headroom">', { value: true }) +
      field('Peak window', 'peak', '<input id="lv-peak" type="range" min="0" max="' + (windows.length - 1) + '" step="1" data-lever="peak">' +
        '<div class="lv-ticks" aria-hidden="true">' + ticks + '</div>', { value: true }) +
      '</div><p class="lv-note" data-note="peak"></p>' +
      '<p class="lv-note">The product uses 10% headroom over the 15-minute peak. A longer window or more headroom is more conservative and saves less.</p></div>';
  }

  function label(minutes) { return minutes < 60 ? minutes + ' min' : ui.num(minutes / 60, 2) + ' h'; }

  function pricing() {
    var c = view.cloud;
    return '<div class="card"><h2>Pricing</h2><div class="sub">What a core-hour and a GiB-hour cost</div>' +
      '<div class="lv-grid">' +
      field('Cloud', 'cloud', '<select id="lv-cloud" data-lever="cloud">' + options(cloudOptions()) + '</select>', { reset: false }) +
      field('Region', 'region', '<select id="lv-region" data-lever="region">' + options(regionOptions(c)) + '</select>', { reset: false }) +
      field('Instance type', 'type', '<input id="lv-type" type="search" list="lv-types" autocomplete="off" spellcheck="false" placeholder="Search, e.g. ' + EXAMPLE[c] + '" data-lever="type">' +
        '<datalist id="lv-types">' + typeList(c, view.region) + '</datalist><div class="lv-hint" data-hint="type"></div>', { reset: 'rates', wide: true }) +
      field('$ per core-hour', 'cpu', '<input id="lv-cpu" type="number" min="0" step="any" inputmode="decimal" data-lever="cpu">', { reset: false }) +
      field('$ per GiB-hour', 'gib', '<input id="lv-gib" type="number" min="0" step="any" inputmode="decimal" data-lever="gib">', { reset: false }) +
      field('Spot or committed-use discount', 'discount', '<input id="lv-discount" type="range" min="0" max="80" step="5" data-lever="discount">', { value: true }) +
      field('Annual cloud bill, optional', 'bill', '<input id="lv-bill" type="number" min="0" step="any" inputmode="numeric" placeholder="Not set" data-lever="bill">') +
      field('Loaded engineer cost per year', 'cost', '<input id="lv-cost" type="number" min="0" step="any" inputmode="numeric" data-lever="cost">') +
      '</div><div class="lv-suggest" data-suggest hidden></div><p class="lv-note" data-note="pricing"></p></div>';
  }

  function namespaces(data) {
    var counts = {}, thoras = data.thorasNamespaces || [];
    data.workloads.forEach(function (w) { counts[w.ns] = (counts[w.ns] || 0) + 1; });
    return '<div class="card"><div class="card-head"><div><h2>Namespaces</h2><div class="sub">Include or exclude namespaces from every figure</div></div>' + reset('namespaces') + '</div>' +
      '<div class="lv-chips" role="group" aria-label="Namespaces">' + data.namespaces.map(function (ns) {
        return '<label class="lv-chip"' + (thoras.indexOf(ns) >= 0 ? ' data-thoras' : '') + '><input type="checkbox" value="' + esc(ns) + '" data-lever="ns">' + esc(ns || '(none)') +
          ' <span class="lv-count">' + counts[ns] + '</span></label>';
      }).join('') + '</div>' + (thoras.length ? thorasControl(thoras) : '') + '</div>';
  }

  // Thoras's own namespaces, shown only when the file has some: they run
  // Thoras, not the customer's workloads, so they are left out by default.
  function thorasControl(list) {
    return '<div class="lv-label lv-thoras"><label class="lv-chip"><input type="checkbox" id="lv-thoras" data-lever="thoras">Include Thoras’s own ' + ui.plural(list.length, 'namespace') + '</label>' + reset('thoras') + '</div>' +
      '<p class="lv-note" data-note="thoras"></p>';
  }

  // Clusters, in a file with several. The links on page one set the same filter.
  function clusters(data) {
    return '<div class="card"><div class="card-head"><div><h2>Clusters</h2><div class="sub">Include or exclude clusters from every figure</div></div>' + reset('clusters') + '</div>' +
      '<div class="lv-chips" role="group" aria-label="Clusters">' + data.clusters.map(function (c) {
        return '<label class="lv-chip" title="' + esc(c.name) + '"><input type="checkbox" value="' + esc(c.name) + '" data-lever="cluster">' + esc(c.label || c.name) +
          ' <span class="lv-count">' + c.workloads + '</span></label>';
      }).join('') + '</div></div>';
  }

  function timeRange(choices) {
    var body = choices ?
      '<div class="lv-grid">' +
      '<div class="lv-field"><div class="lv-label"><label for="lv-from">From</label></div><select id="lv-from" data-lever="from"></select></div>' +
      '<div class="lv-field"><div class="lv-label"><label for="lv-to">To</label></div><select id="lv-to" data-lever="to"></select></div></div>' +
      '<p class="lv-note">Whole ' + (choices.unit === 'day' ? 'days' : 'hours') + ', UTC.</p>' :
      '<p class="lv-note">The data is too short to price part of it.</p>';
    return '<div class="card"><div class="card-head"><div><h2>Time range</h2><div class="sub">Price only part of the window</div></div>' + reset('range') + '</div>' + body + '</div>';
  }

  function build(host, ctx) {
    var data = ctx.data, st = ctx.state, picked = st.node && prices.parseKey(st.node);
    var where = picked || prices.guess(data.nodes) || { cloud: 'aws', region: prices.CLOUDS.aws.ref };
    view = { data: data, cloud: where.cloud, region: where.region, choices: levers.rangeChoices(data.fidelity, st.range) };
    host.innerHTML =
      '<div class="lv-head"><p class="lv-lead">Change an assumption and every number in the report recomputes. The settings are in the page link, so a configured view can be shared.</p>' +
      '<button type="button" class="lv-reset-all" data-reset="all">Reset to defaults</button></div>' +
      '<div class="card lv-result"><h2>Result with these settings <span class="pill pill-info" data-changed hidden>Changed from defaults</span></h2><div class="lv-kpis" data-kpis></div></div>' +
      '<div class="pair">' + rightSizing(data) + pricing() + '</div>' +
      (data.multi ? '<div class="pair">' + clusters(data) + namespaces(data) + '</div>' + timeRange(view.choices) :
        '<div class="pair">' + namespaces(data) + timeRange(view.choices) + '</div>') +
      '<p class="lv-link">Link for this view: <span data-current-link></span></p>';
    q(host, '#lv-cloud').value = view.cloud;
    q(host, '#lv-region').value = view.region;
    bind(host, ctx.set);
  }

  function setRegions(host) {
    var region = q(host, '#lv-region');
    region.innerHTML = options(regionOptions(view.cloud));
    if (!prices.CLOUDS[view.cloud].regions[view.region]) view.region = prices.CLOUDS[view.cloud].ref;
    region.value = view.region;
    q(host, '#lv-types').innerHTML = typeList(view.cloud, view.region);
  }

  function positive(v) {
    var n = Number(v);
    return v !== '' && isFinite(n) && n > 0 ? n : null;
  }

  function pickType(host, set) {
    var input = q(host, '#lv-type'), hint = q(host, '[data-hint="type"]'), name = input.value.trim();
    hint.textContent = '';
    if (!name) { set({ rates: null, node: null }); return; }
    var p = prices.lookup(view.cloud, view.region, name);
    if (!p) {
      // A type from another cloud switches the cloud.
      var other = prices.cloudOf(name);
      if (other) {
        view.cloud = other;
        q(host, '#lv-cloud').value = other;
        setRegions(host);
        p = prices.lookup(view.cloud, view.region, name);
      }
    }
    if (!p) { hint.textContent = 'Not in the built-in prices. Enter the rates below instead.'; return; }
    set({ rates: { perCore: p.perCore, perGiB: p.perGiB }, node: prices.key(p) });
  }

  function bind(host, set) {
    var sliderLabel = function (input) {
      var name = input.getAttribute('data-lever');
      var out = q(host, '[data-value="' + name + '"]');
      if (name === 'headroom' || name === 'discount') out.textContent = input.value + '%';
      if (name === 'peak') out.textContent = label(levers.PEAK_WINDOWS[input.value]);
    };
    host.addEventListener('input', function (e) {
      if (e.target.type === 'range') sliderLabel(e.target);
      // Choosing from the list fires input only. A typed name commits on
      // change, so n1-standard-16 does not commit n1-standard-1 on the way.
      if (e.target.id === 'lv-type' && (!e.inputType || e.inputType === 'insertReplacementText') && prices.cloudOf(e.target.value.trim())) pickType(host, set);
    });
    host.addEventListener('change', function (e) {
      var el = e.target, name = el.getAttribute('data-lever');
      if (name === 'headroom') set({ headroom: Number(el.value) / 100 });
      else if (name === 'discount') set({ discount: Number(el.value) / 100 });
      else if (name === 'peak') {
        var allowed = levers.peakWindows(view.data.fidelity.stepSeconds, view.data.fidelity.snapshot);
        var i = Number(el.value);
        // A disabled window moves to the nearest allowed one.
        while (i < allowed.length - 1 && allowed[i].disabled) i++;
        el.value = i;
        sliderLabel(el);
        set({ peakMinutes: levers.PEAK_WINDOWS[i] });
      } else if (name === 'cloud') {
        view.cloud = el.value;
        setRegions(host);
        q(host, '#lv-type').value = '';
        q(host, '#lv-type').placeholder = 'Search, e.g. ' + EXAMPLE[view.cloud];
        q(host, '[data-hint="type"]').textContent = '';
      } else if (name === 'region') {
        view.region = el.value;
        q(host, '#lv-types').innerHTML = typeList(view.cloud, view.region);
        if (q(host, '#lv-type').value.trim()) pickType(host, set);
      } else if (name === 'type') pickType(host, set);
      else if (name === 'cpu' || name === 'gib') {
        var cpu = positive(q(host, '#lv-cpu').value.trim()), gib = positive(q(host, '#lv-gib').value.trim());
        var blank = !q(host, '#lv-cpu').value.trim() && !q(host, '#lv-gib').value.trim();
        if (cpu && gib) set({ rates: { perCore: cpu, perGiB: gib }, node: null });
        else if (blank) set({ rates: null, node: null });
      } else if (name === 'bill') set({ bill: positive(el.value.trim()) });
      else if (name === 'cost') {
        var cost = positive(el.value.trim());
        if (cost) set({ engineerCost: cost });
      } else if (name === 'cluster') {
        var names = view.data.clusters.map(function (c) { return c.name; });
        set({ onlyClusters: levers.toggleNamespace(view.current.onlyClusters, names, el.value, el.checked) });
      } else if (name === 'ns') set({ namespaces: levers.toggleNamespace(view.current.namespaces, levers.shownNamespaces(view.data, view.current), el.value, el.checked) });
      else if (name === 'thoras') set({ includeThoras: el.checked });
      else if (name === 'from' || name === 'to') {
        set({ range: levers.rangeFor(view.choices, Number(q(host, '#lv-from').value), Number(q(host, '#lv-to').value)) });
      }
    });
    host.addEventListener('click', function (e) {
      var b = e.target.closest('[data-reset],[data-use]');
      if (!b) return;
      if (b.hasAttribute('data-use')) {
        var nodes = levers.suggestionNodes(view.data, view.result), r = nodes && prices.fromNodes(nodes);
        if (r) set({ rates: { perCore: r.perCore, perGiB: r.perGiB }, node: null });
        return;
      }
      var which = b.getAttribute('data-reset');
      set(levers.resetPatch(which === 'all' ? Object.keys(levers.LEVERS) : [which], DEFAULTS));
    });
  }

  function rate(v) { return String(Number(v.toPrecision(4))); }

  // Keeps what the reader is typing in the field that has focus.
  function setValue(el, v) {
    if (document.activeElement !== el) el.value = v;
  }

  function update(host, ctx) {
    var st = ctx.state, r = ctx.result, data = ctx.data, approx = ctx.approx;
    var changed = levers.changed(st, DEFAULTS);
    view.current = st;
    view.result = r;

    q(host, '[data-changed]').hidden = !levers.anyChanged(st, DEFAULTS);
    q(host, '[data-reset="all"]').disabled = !levers.anyChanged(st, DEFAULTS);
    q(host, '[data-kpis]').innerHTML = levers.figures(r, st, ui, approx).map(function (f) {
      return '<div class="lv-k' + (f.sep ? ' sep' : '') + '"><div class="lv-k-label">' + esc(f.label) + '</div><div class="lv-k-value">' + esc(f.value) + '</div>' +
        (f.note ? '<div class="lv-k-note">' + esc(f.note) + '</div>' : '') + '</div>';
    }).join('');
    Array.prototype.forEach.call(host.querySelectorAll('.lv-reset[data-reset]'), function (b) {
      b.hidden = !changed[b.getAttribute('data-reset')];
    });

    // Right-sizing.
    var head = q(host, '#lv-headroom');
    head.value = Math.round(st.headroom * 100);
    q(host, '[data-value="headroom"]').textContent = ui.num(st.headroom * 100, 1) + '%';
    var peak = q(host, '#lv-peak'), windows = levers.peakWindows(data.fidelity.stepSeconds, data.fidelity.snapshot);
    peak.value = levers.peakIndex(st.peakMinutes);
    peak.disabled = windows.every(function (w) { return w.disabled; });
    q(host, '[data-value="peak"]').textContent = label(st.peakMinutes);
    var step = data.fidelity.stepSeconds, note = q(host, '[data-note="peak"]');
    note.textContent = data.fidelity.snapshot ? 'A single snapshot has no history to find peaks in.' :
      windows.some(function (w) { return w.disabled; }) ? 'Windows shorter than the data\'s ' + SP.model.formatStep(step) + ' step are off: the data cannot show peaks that short.' : '';
    note.hidden = !note.textContent;

    // Pricing.
    var picked = st.rates && st.node ? prices.parseKey(st.node) : null;
    if (picked && (Math.abs(picked.perCore - st.rates.perCore) > picked.perCore * 1e-4 || Math.abs(picked.perGiB - st.rates.perGiB) > picked.perGiB * 1e-4)) picked = null;
    var type = q(host, '#lv-type');
    // The picker follows a new pick, or one from a link, but not a cloud or
    // region the reader chose since.
    if (picked && st.node !== view.synced && (picked.cloud !== view.cloud || picked.region !== view.region)) {
      view.cloud = picked.cloud; view.region = picked.region;
      q(host, '#lv-cloud').value = view.cloud;
      setRegions(host);
    }
    view.synced = st.node;
    setValue(type, picked && picked.cloud === view.cloud && picked.region === view.region ? picked.type : '');
    type.placeholder = 'Search, e.g. ' + EXAMPLE[view.cloud];
    q(host, '[data-hint="type"]').textContent = '';
    var cpu = q(host, '#lv-cpu'), gib = q(host, '#lv-gib'), base = r.basePricing || data.pricing;
    setValue(cpu, st.rates ? rate(st.rates.perCore) : '');
    setValue(gib, st.rates ? rate(st.rates.perGiB) : '');
    cpu.placeholder = rate(base.perCore) + ' from nodes';
    gib.placeholder = rate(base.perGiB) + ' from nodes';
    q(host, '#lv-discount').value = Math.round(st.discount * 100);
    q(host, '[data-value="discount"]').textContent = ui.num(st.discount * 100, 1) + '%';
    setValue(q(host, '#lv-bill'), st.bill ? String(st.bill) : '');
    setValue(q(host, '#lv-cost'), String(st.engineerCost));

    var basis = picked ?
      prices.CLOUDS[picked.cloud].name + ' ' + picked.type + ' in ' + picked.region + ': $' + picked.hourly.toFixed(4) + ' per hour for ' +
        picked.cores + ' vCPU and ' + ui.num(picked.gib, 2) + ' GiB' + (picked.scaled ? ', list price scaled from ' + prices.CLOUDS[picked.cloud].ref : ', list price') +
        (st.discount ? '; ' + ui.num(st.discount * 100, 1) + '% discount applied' : '') :
      r.pricing.basis;
    q(host, '[data-note="pricing"]').textContent = 'Priced with ' + basis + '.';

    // Rates from the nodes' own clouds and regions, when they differ from the default pricing.
    var mine = levers.suggestionNodes(data, r), own = mine && prices.fromNodes(mine), suggest = q(host, '[data-suggest]');
    var differs = own && (Math.abs(own.perCore - base.perCore) > base.perCore * 0.005 || Math.abs(own.perGiB - base.perGiB) > base.perGiB * 0.005);
    var using = own && st.rates && Math.abs(st.rates.perCore - own.perCore) < own.perCore * 1e-3 && Math.abs(st.rates.perGiB - own.perGiB) < own.perGiB * 1e-3;
    suggest.hidden = !differs || using;
    if (differs && !using) {
      suggest.innerHTML = 'Priced by their own cloud and region, ' + own.priced + ' of ' + own.nodes + ' ' + ui.plural(own.nodes, 'node') + ' come to $' + esc(rate(own.perCore)) +
        ' per core-hour and $' + esc(rate(own.perGiB)) + ' per GiB-hour. <button type="button" class="link" data-use>Use these rates</button>';
    }

    // Namespaces. Thoras's own are not offered while they are left out.
    var on = st.namespaces;
    var boxes = host.querySelectorAll('input[data-lever="ns"]');
    Array.prototype.forEach.call(boxes, function (b) {
      b.checked = !on || on.indexOf(b.value) >= 0;
      b.disabled = !!on && on.length === 1 && b.checked;
      b.parentNode.classList.toggle('off', !b.checked);
      b.parentNode.hidden = b.parentNode.hasAttribute('data-thoras') && !st.includeThoras;
    });
    var thoras = q(host, '#lv-thoras');
    if (thoras) {
      thoras.checked = !!st.includeThoras;
      thoras.parentNode.classList.toggle('off', !thoras.checked);
      q(host, '[data-note="thoras"]').textContent = st.includeThoras ?
        'Thoras’s own ' + ui.plural(data.thorasLabels.length, 'namespace') + ' (' + data.thorasLabels.join(', ') + ') ' + (data.thorasLabels.length === 1 ? 'is' : 'are') + ' counted.' :
        ui.thorasNote(data.thorasLabels) + ' ' + (data.thorasLabels.length === 1 ? 'It runs' : 'They run') + ' Thoras, not your workloads.';
    }

    // Clusters.
    var only = st.onlyClusters;
    Array.prototype.forEach.call(host.querySelectorAll('input[data-lever="cluster"]'), function (b) {
      b.checked = !only || only.indexOf(b.value) >= 0;
      b.disabled = !!only && only.length === 1 && b.checked;
      b.parentNode.classList.toggle('off', !b.checked);
    });

    // Time range.
    if (view.choices) {
      var from = q(host, '#lv-from'), to = q(host, '#lv-to');
      var a = st.range ? st.range[0] : view.choices.start, b = st.range ? st.range[1] : view.choices.end;
      if (st.range && (!view.choices.from.some(function (o) { return o.value === a; }) || !view.choices.to.some(function (o) { return o.value === b; }))) {
        view.choices = levers.rangeChoices(data.fidelity, st.range);
      }
      from.innerHTML = view.choices.from.map(function (o) { return '<option value="' + o.value + '"' + (o.value >= b ? ' disabled' : '') + '>' + esc(o.label) + '</option>'; }).join('');
      to.innerHTML = view.choices.to.map(function (o) { return '<option value="' + o.value + '"' + (o.value <= a ? ' disabled' : '') + '>' + esc(o.label) + '</option>'; }).join('');
      from.value = String(a);
      to.value = String(b);
    }

    q(host, '[data-current-link]').textContent = location.href;
  }

  function render(host, ctx) {
    if (!view || view.data !== ctx.data || !host.firstChild) build(host, ctx);
    update(host, ctx);
  }

  SP.sections.register({ id: 'assumptions', number: 5, title: 'Assumptions', render: render });
})();
