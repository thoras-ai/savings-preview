/* Section 0: the start screen, the only one shown without data. Takes a file
 * chosen or dropped on the page, or one of the samples the published copy lists. */
(function () {
  'use strict';

  var esc = SP.ui.esc;

  // opts: samples (list of {id, url, name, detail} or null), open(files), openSample(sample)
  function render(host, opts) {
    host.innerHTML =
      '<div class="start">' +
      '<h1>See what Thoras would have saved</h1>' +
      '<p class="lead">Open the metrics file the Savings Preview CLI wrote, or the zip it packed. Choose several to see a fleet of clusters together. Everything is computed in this browser tab. Nothing is uploaded.</p>' +
      '<div class="drop">' +
      '<div class="drop-title">Drop the files here</div>' +
      '<div class="drop-detail"><code>savings-preview.otlp.json</code>, the <code>.zip</code> that contains it, or its <code>report.html</code></div>' +
      '<p class="error" role="alert" hidden></p>' +
      '<label class="button">Choose files<input class="visually-hidden" type="file" multiple accept=".json,.zip,.html,application/json,application/zip,text/html"></label>' +
      '</div>' +
      (opts.samples && opts.samples.length ? '<h2>Or try a sample</h2><ul class="samples">' + opts.samples.map(function (s) {
        return '<li><button type="button" class="sample" data-sample="' + esc(s.id) + '"><span class="sample-name">' + esc(s.name) +
          '</span><span class="sample-detail">' + esc(s.detail) + '</span></button></li>';
      }).join('') + '</ul>' : '') +
      '<p class="start-note">The CLI reads existing history from Prometheus, Datadog, Grafana Cloud or live polling, and writes one OpenTelemetry metrics file. It installs nothing in the cluster.</p>' +
      '</div>';

    var input = host.querySelector('input[type=file]');
    input.addEventListener('change', function () { if (input.files.length) opts.open(input.files); });
    var list = host.querySelector('.samples');
    if (list) list.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-sample]');
      if (!b) return;
      var id = b.getAttribute('data-sample');
      opts.openSample(opts.samples.filter(function (s) { return s.id === id; })[0]);
    });
  }

  function showError(host, message) {
    var el = host.querySelector('.error');
    el.textContent = message || '';
    el.hidden = !message;
    host.querySelector('input[type=file]').value = '';
  }

  function dragging(host, over) {
    host.querySelector('.drop').classList.toggle('over', over);
  }

  (self.SP = self.SP || {}).start = { render: render, showError: showError, dragging: dragging };
})();
