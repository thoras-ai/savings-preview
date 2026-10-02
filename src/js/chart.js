/* SVG chart: requested versus target with the gap hatched, and usage under them. */
(function () {
  'use strict';

  var MAX_POINTS = 480;
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  var MODES = {
    spend: { label: 'Spend', a: 'cur', b: 'opt', scale: 8760, aName: 'Current spend', bName: 'Without waste', money: true },
    cpu: { label: 'CPU', a: 'reqCpu', b: 'tgtCpu', scale: 1, aName: 'Requested', bName: 'Usage + 10%', unit: 'cores', use: 'useCpu', peak: 'peakCpu' },
    mem: { label: 'Memory', a: 'reqMem', b: 'tgtMem', scale: 1, aName: 'Requested', bName: 'Usage + 10%', unit: 'GiB', use: 'useMem', peak: 'peakMem' }
  };

  function legend(modeKey) {
    var mode = MODES[modeKey];
    var items = [{ kind: 'a', name: mode.aName }, { kind: 'b', name: mode.bName }];
    if (mode.peak) items.push({ kind: 'p', name: 'Peak usage' }, { kind: 'u', name: 'Mean usage' });
    items.push({ kind: 'gap', name: mode.money ? 'Waste' : 'Gap' });
    return items;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; });
  }

  function niceStep(span, count) {
    var raw = span / count;
    var pow = Math.pow(10, Math.floor(Math.log10(raw)));
    var f = raw / pow;
    return (f < 1.5 ? 1 : f < 3 ? 2 : f < 7 ? 5 : 10) * pow;
  }

  function fmtMoney(v) {
    var n = Math.abs(v);
    if (n >= 1e6) return '$' + (v / 1e6).toFixed(2) + 'M';
    if (n >= 1e4) return '$' + (v / 1e3).toFixed(1) + 'K';
    return '$' + Math.round(v).toLocaleString('en-US');
  }

  function fmtNum(v, digits) {
    return v.toLocaleString('en-US', { maximumFractionDigits: digits === undefined ? 1 : digits });
  }

  function fmtDate(sec, withTime) {
    var d = new Date(sec * 1000);
    var s = MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate();
    if (withTime) s += ' ' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()) + ' UTC';
    return s;
  }

  function pad(n) { return n < 10 ? '0' + n : String(n); }

  function fmtTick(sec, spanSeconds) {
    var d = new Date(sec * 1000);
    if (spanSeconds <= 2 * 86400) return pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes());
    return MONTHS[d.getUTCMonth()] + ' ' + d.getUTCDate();
  }

  // Fold the series into at most MAX_POINTS buckets, averaging the valid points
  // in each. Peak usage keeps the highest point, so folding never hides a peak.
  function downsample(series, mode) {
    var n = series.t.length;
    var size = Math.max(1, Math.ceil(n / MAX_POINTS));
    var out = { t: [], a: [], b: [], u: [], p: [], size: size };
    for (var i = 0; i < n; i += size) {
      var end = Math.min(n, i + size);
      var sa = 0, sb = 0, su = 0, pk = -Infinity, c = 0;
      for (var j = i; j < end; j++) {
        if (!isFinite(series[mode.a][j])) continue;
        sa += series[mode.a][j]; sb += series[mode.b][j];
        if (mode.use) su += series[mode.use][j];
        if (mode.peak) pk = Math.max(pk, series[mode.peak][j]);
        c++;
      }
      out.t.push(series.t[i]);
      out.a.push(c ? sa / c * mode.scale : NaN);
      out.b.push(c ? sb / c * mode.scale : NaN);
      out.u.push(c && mode.use ? su / c : NaN);
      out.p.push(c && mode.peak ? pk : NaN);
    }
    return out;
  }

  function valueText(mode, v) {
    return mode.money ? fmtMoney(v) + ' per year' : fmtNum(v, 2) + ' ' + mode.unit;
  }

  // opts: title, height, fromZero, names ({a, b} to rename those series),
  // className (added to the svg) and markers ([{t, label}], vertical lines at
  // those times, such as OOM kills).
  function render(host, series, modeKey, opts) {
    opts = opts || {};
    var mode = opts.names ? Object.assign({}, MODES[modeKey], { aName: opts.names.a || MODES[modeKey].aName, bName: opts.names.b || MODES[modeKey].bName }) : MODES[modeKey];
    var width = Math.max(320, host.clientWidth || 800);
    var height = opts.height || 340;
    // Pattern ids are document-wide, and a pattern inside a hidden chart does
    // not paint, so a chart shown only in print passes its own id.
    var hatchId = opts.patternId || 'hatch';
    var d = downsample(series, mode);
    var n = d.t.length;
    var vals = [];
    for (var i = 0; i < n; i++) {
      if (!isFinite(d.a[i])) continue;
      vals.push(d.a[i], d.b[i]);
      if (isFinite(d.p[i])) vals.push(d.p[i]);
    }
    if (!vals.length) { host.innerHTML = '<p class="empty">No complete data to chart.</p>'; return; }
    var lo = Math.min.apply(null, vals), hi = Math.max.apply(null, vals);
    var pad0 = (hi - lo) * 0.12 || hi * 0.1 || 1;
    // opts.fromZero shows amounts against zero, so a gap reads as a share of the whole.
    var yMin = opts.fromZero ? 0 : Math.max(0, lo - pad0), yMax = hi + pad0;
    var step = niceStep(yMax - yMin, 5);
    yMin = Math.max(0, Math.floor(yMin / step) * step);
    yMax = Math.ceil(yMax / step) * step;
    var ticks = [];
    for (var tv = yMin; tv <= yMax + step / 2; tv += step) ticks.push(tv);
    var yLabels = ticks.map(function (v) { return mode.money ? fmtMoney(v) : fmtNum(v, step < 1 ? 2 : 1); });
    var left = 12 + Math.max.apply(null, yLabels.map(function (s) { return s.length; })) * 7.4;
    var m = { top: 12, right: 16, bottom: 30, left: left };
    var w = width - m.left - m.right, h = height - m.top - m.bottom;
    var t0 = d.t[0], t1 = d.t[n - 1];
    var span = t1 - t0;
    function x(i) { return m.left + (n > 1 ? (d.t[i] - t0) / span * w : w / 2); }
    function y(v) { return m.top + h - (v - yMin) / (yMax - yMin) * h; }

    var aPath = '', bPath = '', uPath = '', pPath = '', gaps = '', dots = '';
    var run = [];
    function flush() {
      if (run.length > 1) {
        var top = run.map(function (i) { return x(i).toFixed(1) + ',' + y(d.a[i]).toFixed(1); });
        var bottom = run.slice().reverse().map(function (i) { return x(i).toFixed(1) + ',' + y(Math.min(d.a[i], d.b[i])).toFixed(1); });
        gaps += '<polygon class="gap" points="' + top.concat(bottom).join(' ') + '"/>';
      }
      run = [];
    }
    var pen = false;
    for (var k = 0; k < n; k++) {
      if (!isFinite(d.a[k])) { pen = false; flush(); continue; }
      var cmd = pen ? 'L' : 'M';
      aPath += cmd + x(k).toFixed(1) + ' ' + y(d.a[k]).toFixed(1);
      bPath += cmd + x(k).toFixed(1) + ' ' + y(d.b[k]).toFixed(1);
      if (mode.peak) {
        uPath += cmd + x(k).toFixed(1) + ' ' + y(d.u[k]).toFixed(1);
        pPath += cmd + x(k).toFixed(1) + ' ' + y(d.p[k]).toFixed(1);
      }
      pen = true;
      run.push(k);
      if (n === 1) {
        dots = '<circle class="dot-a" cx="' + x(k) + '" cy="' + y(d.a[k]) + '" r="4"/><circle class="dot-b" cx="' + x(k) + '" cy="' + y(d.b[k]) + '" r="4"/>';
        if (mode.peak) dots += '<circle class="dot-u" cx="' + x(k) + '" cy="' + y(d.u[k]) + '" r="3"/><circle class="dot-p" cx="' + x(k) + '" cy="' + y(d.p[k]) + '" r="4"/>';
      }
    }
    flush();

    var grid = ticks.map(function (v, i) {
      return '<line class="grid" x1="' + m.left + '" x2="' + (m.left + w) + '" y1="' + y(v).toFixed(1) + '" y2="' + y(v).toFixed(1) + '"/>' +
        '<text class="axis" x="' + (m.left - 8) + '" y="' + (y(v) + 4).toFixed(1) + '" text-anchor="end">' + esc(yLabels[i]) + '</text>';
    }).join('');

    var xt = '';
    var unit = span <= 2 * 86400 ? 3600 : 86400;
    var maxLabels = Math.max(2, Math.floor(w / 90));
    var every = Math.max(1, Math.ceil((span / unit) / maxLabels));
    for (var tt = Math.ceil(t0 / unit) * unit, idx = 0; tt <= t1 && n > 1; tt += unit, idx++) {
      if (idx % every) continue;
      var px = m.left + (tt - t0) / span * w;
      xt += '<text class="axis" x="' + px.toFixed(1) + '" y="' + (height - 8) + '" text-anchor="middle">' + esc(fmtTick(tt, span)) + '</text>';
    }
    if (n === 1) xt = '<text class="axis" x="' + (m.left + w / 2) + '" y="' + (height - 8) + '" text-anchor="middle">' + esc(fmtDate(t0, true)) + '</text>';

    var marks = (opts.markers || []).filter(function (mk) { return n > 1 && mk.t >= t0 && mk.t <= t1 + series.step; }).map(function (mk) {
      var px = (m.left + Math.min(1, (mk.t - t0) / span) * w).toFixed(1);
      return '<g class="marker"><line x1="' + px + '" x2="' + px + '" y1="' + m.top + '" y2="' + (m.top + h) + '"/>' +
        '<circle cx="' + px + '" cy="' + m.top + '" r="4"><title>' + esc(mk.label) + '</title></circle></g>';
    }).join('');

    host.innerHTML =
      '<svg class="chart' + (opts.className ? ' ' + esc(opts.className) : '') + '" width="' + width + '" height="' + height + '" viewBox="0 0 ' + width + ' ' + height + '" role="img" aria-label="' +
      esc((opts.title || 'Chart') + ': ' + [mode.aName, mode.bName].concat(mode.peak ? ['Peak usage', 'Mean usage'] : []).join(', ')) + '">' +
      '<defs><pattern id="' + hatchId + '" width="3" height="3" patternUnits="userSpaceOnUse" patternTransform="rotate(-45)">' +
      '<line x1="0" y1="0" x2="0" y2="3" class="hatch-line" stroke-width="1"/></pattern></defs>' +
      grid + xt + gaps.replace(/class="gap"/g, 'class="gap" fill="url(#' + hatchId + ')"') +
      (mode.peak ? '<path class="line-u" d="' + uPath + '"/><path class="line-p" d="' + pPath + '"/>' : '') +
      '<path class="line-a" d="' + aPath + '"/><path class="line-b" d="' + bPath + '"/>' + dots +
      '<line class="cursor" y1="' + m.top + '" y2="' + (m.top + h) + '" x1="0" x2="0" visibility="hidden"/>' +
      '<rect class="hit" x="' + m.left + '" y="' + m.top + '" width="' + w + '" height="' + h + '" fill="transparent"/>' + marks + '</svg>' +
      '<div class="tip" hidden></div>';

    var svg = host.querySelector('svg'), tip = host.querySelector('.tip');
    var cursor = svg.querySelector('.cursor');
    function show(ev) {
      var r = svg.getBoundingClientRect();
      var px = ev.clientX - r.left;
      var best = -1, bd = Infinity;
      for (var i = 0; i < n; i++) {
        if (!isFinite(d.a[i])) continue;
        var dist = Math.abs(x(i) - px);
        if (dist < bd) { bd = dist; best = i; }
      }
      if (best < 0) return;
      cursor.setAttribute('x1', x(best)); cursor.setAttribute('x2', x(best)); cursor.setAttribute('visibility', 'visible');
      var rows = '<div class="tip-time">' + esc(fmtDate(d.t[best], true)) + '</div>' +
        row('a', mode.aName, valueText(mode, d.a[best])) + row('b', mode.bName, valueText(mode, d.b[best]));
      if (mode.peak) rows += row('p', 'Peak usage', valueText(mode, d.p[best])) + row('u', 'Mean usage', valueText(mode, d.u[best]));
      rows += '<div class="tip-row tip-total"><span>' + (mode.money ? 'Waste' : 'Gap') + '</span><span>' +
        valueText(mode, Math.max(0, d.a[best] - d.b[best])) + '</span></div>';
      var until = best + 1 < n ? d.t[best + 1] : d.t[best] + d.size * series.step;
      (opts.markers || []).forEach(function (mk) {
        if (mk.t >= d.t[best] && mk.t < until) rows += '<div class="tip-row tip-marker"><span>' + esc(mk.label) + '</span></div>';
      });
      tip.innerHTML = rows;
      tip.hidden = false;
      var tw = tip.offsetWidth;
      var left = x(best) + 14;
      if (left + tw > width) left = x(best) - tw - 14;
      tip.style.left = Math.max(0, left) + 'px';
      tip.style.top = m.top + 'px';
    }
    function row(kind, name, value) {
      return '<div class="tip-row"><span><i class="swatch swatch-' + kind + '"></i>' + esc(name) + '</span><span>' + esc(value) + '</span></div>';
    }
    svg.addEventListener('mousemove', show);
    svg.addEventListener('mouseleave', function () { tip.hidden = true; cursor.setAttribute('visibility', 'hidden'); });
  }

  var api = { render: render, modes: MODES, legend: legend, fmtMoney: fmtMoney, fmtNum: fmtNum, downsample: downsample };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).chart = api;
})();
