/* Page wiring: loads the contract file once, keeps the shared state in the URL
 * hash, recomputes the figures when a lever changes and renders each section. */
(function () {
  'use strict';

  var model = SP.model, files = SP.files, state = SP.state, ui = SP.ui, sections = SP.sections.list, start = SP.start;
  var DATA_URL = new URLSearchParams(location.search).get('data') || 'savings-preview.otlp.json';
  var THEME_KEY = 'savings-preview-theme';
  var store = state.store(state.parse(location.hash));
  var page = { data: null, result: null, theme: 'auto', samples: null };

  function $(id) { return document.getElementById(id); }

  // The page opens in the light theme until the reader picks another.
  // Storage can be missing or blocked (private windows, pages opened from disk).
  function storedTheme() {
    try {
      var v = localStorage.getItem(THEME_KEY);
      return v === 'auto' || v === 'dark' ? v : 'light';
    } catch (e) { return 'light'; }
  }

  function setTheme(choice) {
    page.theme = choice;
    try {
      if (choice === 'light') localStorage.removeItem(THEME_KEY);
      else localStorage.setItem(THEME_KEY, choice);
    } catch (e) { /* the choice lasts until the page closes */ }
    applyTheme();
  }

  function applyTheme() {
    var dark = page.theme === 'dark' ||
      (page.theme === 'auto' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-choice]'), function (b) {
      b.setAttribute('aria-checked', String(b.getAttribute('data-theme-choice') === page.theme));
    });
  }

  // A page with data-samples is the published copy: it opens on the upload
  // screen and lists sample files next to it.
  function samples() {
    var raw = document.body.getAttribute('data-samples');
    if (!raw) return null;
    try { return JSON.parse(raw); } catch (e) { return null; }
  }

  function linkedSample() {
    var id = store.get().sample;
    return id && page.samples ? page.samples.filter(function (s) { return s.id === id; })[0] : null;
  }

  function show(which) {
    $('loading').hidden = which !== 'loading';
    $('start').hidden = which !== 'start';
    $('report').hidden = which !== 'report';
    $('open-another').hidden = which !== 'report';
    $('copy-link').hidden = which !== 'report';
    $('print-summary').hidden = which !== 'report';
    $('book-demo').hidden = which !== 'report';
    // Printing shows the summary layout only while a report is open.
    document.body.classList.toggle('has-print', which === 'report');
    if (which !== 'report') $('context').textContent = '';
  }

  function showError(message) {
    show('start');
    start.showError($('start'), message);
  }

  function writeHash(st) {
    var h = state.serialize(st);
    try { history.replaceState(null, '', location.pathname + location.search + (h ? '#' + h : '')); } catch (e) { /* not allowed here */ }
    Array.prototype.forEach.call(document.querySelectorAll('[data-current-link]'), function (el) { el.textContent = location.href; });
    // Each section link carries the settings, so it opens the same view in a new tab.
    Array.prototype.forEach.call($('secbar').querySelectorAll('a[data-section]'), function (a) {
      a.setAttribute('href', '#' + state.serialize(Object.assign({}, st, { section: a.getAttribute('data-section') })));
    });
  }

  // Settings from a link made for another file: namespaces and a workload
  // this file does not have are dropped.
  function fitToData(st, data) {
    var patch = {};
    if (st.namespaces) {
      var known = st.namespaces.filter(function (n) { return data.namespaces.indexOf(n) >= 0; });
      if (known.length !== st.namespaces.length) patch.namespaces = known.length ? known : null;
    }
    if (st.workload && !data.workloads.some(function (w) { return w.id === st.workload; })) patch.workload = null;
    return patch;
  }

  function context() {
    return {
      data: page.data, result: page.result, state: store.get(), changed: null, set: store.set, go: go
    };
  }

  function recompute() {
    var t0 = performance.now();
    page.result = model.compute(page.data, state.levers(store.get()));
    page.timing.compute = performance.now() - t0;
  }

  function renderSections(changed) {
    var t0 = performance.now(), ctx = context();
    ctx.changed = changed;
    sections.forEach(function (s) { s.render($('section-' + s.id).querySelector('.section-body'), ctx); });
    page.timing.render = performance.now() - t0;
  }

  // The summary is page one. The other sections sit in one collapsed
  // disclosure under it, with their own section bar.
  function buildLayout() {
    var rest = sections.slice(1);
    function section(s) {
      // The summary opens with its headline, so its title is for screen readers only.
      var head = s.id === 'summary' ? '<h2 class="visually-hidden">' + ui.esc(s.title) + '</h2>' :
        '<header class="section-head"><span class="step">' + s.number + '</span><h2>' + ui.esc(s.title) + '</h2></header>';
      return '<section id="section-' + s.id + '" class="section' + (s.placeholder ? ' section-placeholder' : '') + '" data-section="' + s.id + '">' +
        head + '<div class="section-body"></div></section>';
    }
    $('report').innerHTML = section(sections[0]) +
      '<details id="details" class="details"><summary>Details for engineers</summary>' +
      '<nav id="secbar" class="secbar" aria-label="Report sections"><ol>' + rest.map(function (s) {
        return '<li><a href="#section=' + s.id + '" data-section="' + s.id + '"><span class="step">' + s.number + '</span>' + ui.esc(s.title) + '</a></li>';
      }).join('') + '</ol></nav>' + rest.map(section).join('') + '</details>';
  }

  function markActive(id) {
    Array.prototype.forEach.call($('secbar').querySelectorAll('a[data-section]'), function (a) {
      var on = a.getAttribute('data-section') === id;
      a.classList.toggle('active', on);
      if (on) a.setAttribute('aria-current', 'true'); else a.removeAttribute('aria-current');
    });
  }

  var scrolling = null;
  function go(id, instant) {
    var el = $('section-' + id);
    if (!el) return;
    if (id !== sections[0].id) $('details').open = true;
    scrolling = id;
    store.set({ section: id });
    markActive(id);
    var behavior = instant ? 'auto' : 'smooth';
    if (id === sections[0].id) window.scrollTo({ top: 0, behavior: behavior });
    else el.scrollIntoView({ behavior: behavior, block: 'start' });
    settle();
  }

  // A smooth scroll sends scroll events until it ends; the clicked section
  // stays marked until they stop.
  var settleTimer;
  function settle() {
    clearTimeout(settleTimer);
    settleTimer = setTimeout(function () { scrolling = null; }, 200);
  }

  // The section in view is the last one whose top has passed under the bar,
  // or the last section once the page is scrolled to the end.
  function sectionInView() {
    if (!$('details').open) return sections[0].id;
    var bar = $('secbar').getBoundingClientRect().bottom, id = sections[0].id;
    sections.forEach(function (s) {
      if ($('section-' + s.id).getBoundingClientRect().top <= bar + 16) id = s.id;
    });
    if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) id = sections[sections.length - 1].id;
    return id;
  }

  var ticking = false;
  function onScroll() {
    if (ticking || $('report').hidden) return;
    ticking = true;
    requestAnimationFrame(function () {
      ticking = false;
      if (scrolling) { settle(); return; }
      var id = sectionInView();
      markActive(id);
      store.set({ section: id });
    });
  }

  function load(doc) {
    var t0 = performance.now(), data;
    try { data = model.prepare(doc); } catch (e) { showError(e.message); return; }
    // Cleared first, so the store does not recompute the previous file.
    page.data = null;
    store.set(fitToData(store.get(), data));
    writeHash(store.get());
    page.data = data;
    page.timing = { prepare: performance.now() - t0 };
    recompute();
    show('report');
    $('context').textContent = data.cluster || 'Unnamed cluster';
    renderSections(null);
    var id = store.get().section;
    markActive(id);
    if (id !== state.DEFAULTS.section) go(id, true); else window.scrollTo(0, 0);
    page.timing.total = performance.now() - t0;
    page.timing.sinceOpen = performance.now();
  }

  function fetchData() {
    show('loading');
    fetch(DATA_URL).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(load).catch(function () { showError(''); });
  }

  function readFile(file) {
    store.set({ sample: null });
    show('loading');
    files.read(file).then(load, function (e) { showError(e.message); });
  }

  // The sample's id goes in the link, so the link opens the same sample.
  function openSample(s) {
    store.set({ sample: s.id });
    show('loading');
    fetch(s.url).then(function (r) {
      if (!r.ok) throw new Error('The sample did not load (HTTP ' + r.status + ').');
      return r.arrayBuffer();
    }).then(function (buf) { return files.fromBytes(new Uint8Array(buf)); })
      .then(load, function (e) { showError(e.message); });
  }

  function bind() {
    store.subscribe(function (st, changed) {
      writeHash(st);
      if (!page.data || (changed.length === 1 && changed[0] === 'section')) return;
      if (state.affectsFigures(changed)) recompute();
      renderSections(changed);
    });
    window.addEventListener('hashchange', function () {
      var next = state.parse(location.hash), before = store.get().section;
      if (page.data) Object.assign(next, fitToData(next, page.data));
      store.set(next);
      if (page.data && next.section !== before) go(next.section);
    });
    $('secbar').addEventListener('click', function (e) {
      var a = e.target.closest('a[data-section]');
      if (!a) return;
      e.preventDefault();
      go(a.getAttribute('data-section'));
    });
    $('report').addEventListener('click', function (e) {
      var a = e.target.closest('a[data-go]');
      if (!a) return;
      e.preventDefault();
      go(a.getAttribute('data-go'));
    });
    window.addEventListener('scroll', onScroll, { passive: true });
    ['dragover', 'dragleave', 'drop'].forEach(function (ev) {
      document.addEventListener(ev, function (e) {
        e.preventDefault();
        start.dragging($('start'), ev === 'dragover');
        if (ev === 'drop' && e.dataTransfer.files[0]) readFile(e.dataTransfer.files[0]);
      });
    });
    $('open-another').addEventListener('click', function () { showError(''); window.scrollTo(0, 0); });
    $('copy-link').addEventListener('click', copyLink);
    $('print-summary').addEventListener('click', function () { window.print(); });
    // The print layout is drawn only when it is needed, so lever changes do not pay for it.
    window.addEventListener('beforeprint', function () {
      if (page.result && !$('report').hidden) SP.print.render($('print'), context());
    });
    $('theme').addEventListener('click', function (e) {
      var b = e.target.closest('button[data-theme-choice]');
      if (b) setTheme(b.getAttribute('data-theme-choice'));
    });
    var timer;
    window.addEventListener('resize', function () {
      clearTimeout(timer);
      timer = setTimeout(function () {
        if (!page.result || $('report').hidden) return;
        resizeSections();
      }, 120);
    });
    // Charts drawn while the details were closed had no width to fit.
    $('details').addEventListener('toggle', function () {
      if (!page.result) return;
      if ($('details').open) resizeSections();
      else if (store.get().section !== sections[0].id) { store.set({ section: sections[0].id }); markActive(sections[0].id); }
    });
    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: dark)');
      if (mq.addEventListener) mq.addEventListener('change', applyTheme);
    }
  }

  function resizeSections() {
    var ctx = context();
    sections.forEach(function (s) { if (s.resize) s.resize($('section-' + s.id).querySelector('.section-body'), ctx); });
  }

  // The link carries the settings in its hash. The clipboard API needs a
  // secure context, so a page served over plain HTTP falls back to a selection.
  function copyLink() {
    var b = $('copy-link'), url = location.href;
    function done(ok) {
      b.textContent = ok ? 'Copied' : 'Copy failed';
      clearTimeout(copyLink.timer);
      copyLink.timer = setTimeout(function () { b.textContent = 'Copy link'; }, 1500);
    }
    function fallback() {
      var t = document.createElement('textarea');
      t.value = url; t.setAttribute('readonly', ''); t.style.position = 'fixed'; t.style.opacity = '0';
      document.body.appendChild(t); t.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { /* not supported */ }
      document.body.removeChild(t);
      done(ok);
    }
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(url).then(function () { done(true); }, fallback);
    else fallback();
  }

  // A copy saved for sharing carries the contract file in the page itself.
  function inlineData() {
    var el = document.getElementById('report-data');
    if (!el) return null;
    try { return JSON.parse(el.textContent); } catch (e) { return null; }
  }

  page.theme = storedTheme();
  applyTheme();
  buildLayout();
  page.samples = samples();
  start.render($('start'), { samples: page.samples, open: readFile, openSample: openSample });
  bind();
  // Load and render times, for checking the page stays fast on large files.
  SP.timing = function () { return page.timing; };
  var inline = inlineData();
  if (inline) load(inline);
  else if (page.samples && linkedSample()) openSample(linkedSample());
  else if (page.samples || location.protocol === 'file:') showError('');
  else fetchData();
})();
