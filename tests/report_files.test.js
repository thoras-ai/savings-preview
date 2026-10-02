'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const { execFileSync } = require('node:child_process');
const files = require('../src/js/files.js');

const doc = { resourceMetrics: [{ resource: { attributes: [{ key: 'k8s.cluster.name', value: { stringValue: 'a</script>&b' } }] } }] };

// A zip with the given entries, deflated unless stored is set, as archive/zip writes them.
function zip(entries, stored) {
  const locals = [], central = [];
  let offset = 0;
  for (const [name, text] of entries) {
    const raw = Buffer.from(text), body = stored ? raw : zlib.deflateRawSync(raw), n = Buffer.from(name);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0); local.writeUInt16LE(stored ? 0 : 8, 8);
    local.writeUInt32LE(body.length, 18); local.writeUInt32LE(raw.length, 22); local.writeUInt16LE(n.length, 26);
    const cd = Buffer.alloc(46);
    cd.writeUInt32LE(0x02014b50, 0); cd.writeUInt16LE(stored ? 0 : 8, 10);
    cd.writeUInt32LE(body.length, 20); cd.writeUInt32LE(raw.length, 24); cd.writeUInt16LE(n.length, 28); cd.writeUInt32LE(offset, 42);
    locals.push(local, n, body); central.push(cd, n);
    offset += 30 + n.length + body.length;
  }
  const cdBuf = Buffer.concat(central), end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0); end.writeUInt16LE(entries.length, 8); end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(cdBuf.length, 12); end.writeUInt32LE(offset, 16);
  return new Uint8Array(Buffer.concat([...locals, cdBuf, end]));
}

// The page with data inlined the way the CLI writes report.html.
function inlined(page, data) {
  const escaped = JSON.stringify(data).replace(/[<>&]/g, (c) => '\\u00' + c.charCodeAt(0).toString(16));
  const i = page.lastIndexOf('<script>');
  return page.slice(0, i) + '<script type="application/json" id="report-data">' + escaped + '</script>\n' + page.slice(i);
}

function builtPage() {
  const dir = fs.mkdtempSync(path.join(require('node:os').tmpdir(), 'page-'));
  execFileSync('python3', [path.join(__dirname, '..', 'scripts', 'build_report.py'), dir]);
  return fs.readFileSync(path.join(dir, 'index.html'), 'utf8');
}

test('reads a contract file', async () => {
  assert.deepStrictEqual(await files.fromBytes(new TextEncoder().encode(JSON.stringify(doc))), doc);
});

test('reads the data out of a report page', async () => {
  const page = builtPage();
  assert.deepStrictEqual(await files.fromBytes(new TextEncoder().encode(inlined(page, doc))), doc);
  await assert.rejects(files.fromBytes(new TextEncoder().encode(page)), /no report data/);
});

test('reads report.html from the zip, deflated or stored', async () => {
  const page = inlined('<html><body><script>x()</script></body></html>', doc);
  for (const stored of [false, true]) {
    const z = zip([['summary.json', '{}'], ['report.html', page]], stored);
    assert.deepStrictEqual(await files.fromBytes(z), doc);
  }
});

test('prefers a contract file in the zip', async () => {
  const other = { resourceMetrics: [] };
  const z = zip([['report.html', inlined('<script>x()</script>', doc)], ['data/savings-preview.otlp.json', JSON.stringify(other)]]);
  assert.deepStrictEqual(await files.fromBytes(z), other);
});

test('explains files it cannot read', async () => {
  await assert.rejects(files.fromBytes(new TextEncoder().encode('{nope')), /not valid JSON/);
  await assert.rejects(files.fromBytes(zip([['summary.json', '{}']])), /no report.html/);
  await assert.rejects(files.fromBytes(zip([['report.html', 'x']]).slice(0, 40)), /damaged/);
});
