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
  function fromZip(bytes) {
    return Promise.resolve().then(function () {
      var entries = zipEntries(bytes);
      var base = function (e) { return e.name.split('/').pop(); };
      var pick = entries.filter(function (e) { return /\.otlp\.json$/.test(base(e)); })[0] ||
        entries.filter(function (e) { return /\.html?$/.test(base(e)); })[0];
      if (!pick) fail('That zip has no report.html or .otlp.json file in it.');
      return entryText(bytes, pick).then(fromText);
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

  var api = { read: read, fromBytes: fromBytes, fromText: fromText, fromZip: fromZip, zipEntries: zipEntries };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).files = api;
})();
