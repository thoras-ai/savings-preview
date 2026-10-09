/* Reads the files the page accepts: a contract file, the CLI's savings-preview.zip,
 * or a report page that carries its data. Everything is read in the browser. */
(function () {
  'use strict';

  // Split so the page's own script text never contains the marker it looks for.
  var DATA_START = '<script type="application/json" id="report-' + 'data">';
  var DATA_END = '<\/script>';

  function fail(message) { throw new Error(message); }

  // The page writes its data with <, > and & escaped, so the first closing
  // script tag after the start is the end of the data.
  function fromPage(text) {
    var i = text.indexOf(DATA_START);
    if (i < 0) fail('That page has no report data in it. Open the report.html from savings-preview.zip.');
    i += DATA_START.length;
    var j = text.indexOf(DATA_END, i);
    if (j < 0) fail('The report data in that page is incomplete.');
    return parseJSON(text.slice(i, j));
  }

  function parseJSON(text) {
    try { return JSON.parse(text); } catch (e) { return fail('That file is not valid JSON.'); }
  }

  function fromText(text) {
    var head = text.slice(0, 512).replace(/^﻿/, '').trimStart();
    if (head.charAt(0) === '<') return fromPage(text);
    return parseJSON(text);
  }

  function u16(b, o) { return b[o] | (b[o + 1] << 8); }
  function u32(b, o) { return (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16)) + b[o + 3] * 16777216; }
  function u64(b, o) { return u32(b, o) + u32(b, o + 4) * 4294967296; }

  function isZip(bytes) { return bytes.length >= 4 && u32(bytes, 0) === 0x04034b50; }

  // Lists the entries of a zip from its central directory.
  function zipEntries(b) {
    var end = -1;
    for (var i = b.length - 22; i >= Math.max(0, b.length - 65557); i--) {
      if (u32(b, i) === 0x06054b50) { end = i; break; }
    }
    if (end < 0) fail('That zip file is damaged or incomplete.');
    var count = u16(b, end + 10), off = u32(b, end + 16), out = [];
    for (var k = 0; k < count; k++) {
      if (off + 46 > b.length || u32(b, off) !== 0x02014b50) fail('That zip file is damaged or incomplete.');
      var e = {
        method: u16(b, off + 10), size: u32(b, off + 20), local: u32(b, off + 42),
        name: new TextDecoder().decode(b.subarray(off + 46, off + 46 + u16(b, off + 28)))
      };
      var x = off + 46 + u16(b, off + 28), xEnd = x + u16(b, off + 30);
      // Zip64 sizes and offsets live in extra field 1, in this order, when the 32-bit field is full.
      while (x + 4 <= xEnd) {
        var id = u16(b, x), len = u16(b, x + 2), p = x + 4;
        if (id === 1) {
          if (u32(b, off + 24) === 0xffffffff) p += 8;
          if (e.size === 0xffffffff) { e.size = u64(b, p); p += 8; }
          if (e.local === 0xffffffff) e.local = u64(b, p);
        }
        x += 4 + len;
      }
      out.push(e);
      off = xEnd + u16(b, off + 32);
    }
    return out;
  }

  function entryText(b, e) {
    var start = e.local + 30 + u16(b, e.local + 26) + u16(b, e.local + 28);
    var data = b.subarray(start, start + e.size);
    if (e.method === 0) return Promise.resolve(new TextDecoder().decode(data));
    if (e.method !== 8) return Promise.reject(new Error('That zip uses a compression method this page cannot read.'));
    if (typeof DecompressionStream === 'undefined') return Promise.reject(new Error('This browser cannot unpack zip files. Open the report.html inside it instead.'));
    var stream = new Blob([data]).stream().pipeThrough(new DecompressionStream('deflate-raw'));
    return new Response(stream).text();
  }

  // Prefers a contract file in the zip, then the report page the CLI puts there.
  // The CLI's zip also carries each cluster's compact file in a NAME.clusters
  // folder. Its report.html holds all of them and the clusters that could not
  // be read, so those files are read, together, only when it is missing.
  function fromZip(bytes) {
    return Promise.resolve().then(function () {
      var entries = zipEntries(bytes);
      var base = function (e) { return e.name.split('/').pop(); };
      var data = entries.filter(function (e) { return /\.otlp\.json$/.test(base(e)); });
      var perCluster = function (e) { return /(^|\/)[^/]+\.clusters\//.test(e.name); };
      var pick = data.filter(function (e) { return !perCluster(e); })[0] ||
        entries.filter(function (e) { return /\.html?$/.test(base(e)); })[0];
      if (pick) return entryText(bytes, pick).then(fromText);
      if (!data.length) fail('That zip has no report.html or .otlp.json file in it.');
      return Promise.all(data.map(function (e) {
        return entryText(bytes, e).then(function (text) { return { name: e.name, doc: fromText(text) }; });
      })).then(function (list) { return combine(list).doc; });
    });
  }

  function fromBytes(bytes) {
    if (isZip(bytes)) return fromZip(bytes);
    return Promise.resolve().then(function () { return fromText(new TextDecoder().decode(bytes)); });
  }

  // Reads a File or Blob and resolves to the contract document.
  function read(file) {
    return file.arrayBuffer().then(function (buf) { return fromBytes(new Uint8Array(buf)); });
  }

  function clusterOf(rm) {
    var list = (rm.resource && rm.resource.attributes) || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i].key === 'k8s.cluster.name' && list[i].value) return String(list[i].value.stringValue || '');
    }
    return '';
  }

  function lastTime(rm) {
    var last = -Infinity;
    (rm.scopeMetrics || []).forEach(function (sm) {
      (sm.metrics || []).forEach(function (m) {
        var pts = ((m.gauge || m.sum || {}).dataPoints) || [];
        if (pts.length) last = Math.max(last, Number(pts[pts.length - 1].timeUnixNano) / 1e9);
      });
    });
    return last;
  }

  // Joins the files the CLI wrote, one cluster or several each, into one
  // document. A cluster in more than one file is taken from the file whose
  // data for it ends latest, the later file on a tie; a file that only
  // records the cluster as not read loses to one with data.
  //   list    [{name, doc}]
  //   result  {doc, dropped: [{cluster, kept, dropped}]}
  function combine(list) {
    if (list.length === 1) return { doc: list[0].doc, dropped: [] };
    var groups = {}, order = [];
    list.forEach(function (f, fi) {
      var rms = (f.doc && f.doc.resourceMetrics) || [];
      if (!Array.isArray(rms) || !rms.length) fail(f.name + ' has no resourceMetrics, so it is not a Savings Preview file.');
      var first = '';
      rms.forEach(function (rm) { if (!first) first = clusterOf(rm); });
      rms.forEach(function (rm) {
        var c = clusterOf(rm) || first;
        var key = fi + '\u0000' + c;
        var g = groups[c];
        if (!g) { g = groups[c] = {}; order.push(c); }
        if (!g[key]) g[key] = { file: f.name, index: fi, end: -Infinity, rms: [] };
        g[key].rms.push(rm);
        g[key].end = Math.max(g[key].end, lastTime(rm));
      });
    });
    var out = [], dropped = [];
    order.forEach(function (c) {
      var cands = Object.keys(groups[c]).map(function (k) { return groups[c][k]; });
      var best = cands.reduce(function (a, b) { return b.end > a.end || (b.end === a.end && b.index > a.index) ? b : a; });
      cands.forEach(function (x) { if (x !== best) dropped.push({ cluster: c, kept: best.file, dropped: x.file }); });
      Array.prototype.push.apply(out, best.rms);
    });
    return { doc: { resourceMetrics: out }, dropped: dropped };
  }

  // Reads several Files and joins them; see combine.
  function readAll(fileList) {
    var list = Array.prototype.slice.call(fileList);
    return Promise.all(list.map(function (f) {
      return read(f).then(function (doc) { return { name: f.name, doc: doc }; }, function (e) { throw new Error(f.name + ': ' + e.message); });
    })).then(combine);
  }

  var api = { read: read, readAll: readAll, combine: combine, fromBytes: fromBytes, fromText: fromText, fromZip: fromZip, zipEntries: zipEntries };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).files = api;
})();
