'use strict';
const test = require('node:test');
const assert = require('node:assert');
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const launcher = require('../packaging/npm/lib/launcher.js');

const TAG = 'v1.2.3';
const PLAT = 'linux_amd64';
const ARCHIVE = `savings-preview_${TAG}_${PLAT}.tar.gz`;

// A release archive laid out as scripts/release.sh packages it, whose binary
// prints its arguments.
function archive(dir) {
  const name = `savings-preview_${TAG}_${PLAT}`;
  fs.mkdirSync(path.join(dir, name));
  fs.writeFileSync(path.join(dir, name, 'savings-preview'), '#!/bin/sh\necho "ran $*"\n', { mode: 0o755 });
  execFileSync('tar', ['-czf', ARCHIVE, name], { cwd: dir });
  return fs.readFileSync(path.join(dir, ARCHIVE));
}

function sha(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

function listen(handler) {
  return new Promise((resolve) => {
    const server = http.createServer(handler).listen(0, '127.0.0.1', () => {
      resolve({ server, url: `http://127.0.0.1:${server.address().port}` });
    });
  });
}

// release serves the assets the way GitHub does: public download URLs, and an
// API that redirects asset downloads to a storage host on another origin.
async function release(assets) {
  const seen = [];
  const storage = await listen((req, res) => {
    seen.push({ host: 'storage', url: req.url, auth: req.headers.authorization });
    const name = decodeURIComponent(req.url.slice(1));
    if (!(name in assets)) { res.writeHead(404).end(); return; }
    res.writeHead(200).end(assets[name]);
  });
  const web = await listen((req, res) => {
    seen.push({ host: 'web', url: req.url, auth: req.headers.authorization });
    const prefix = `/thoras-ai/savings-preview/releases/download/${TAG}/`;
    const name = req.url.startsWith(prefix) ? req.url.slice(prefix.length) : '';
    if (!(name in assets)) { res.writeHead(404).end(); return; }
    res.writeHead(302, { location: `${storage.url}/${name}` }).end();
  });
  const api = await listen((req, res) => {
    seen.push({ host: 'api', url: req.url, auth: req.headers.authorization, accept: req.headers.accept });
    if (req.headers.authorization !== 'Bearer secret') { res.writeHead(404).end(); return; }
    if (req.url === `/repos/thoras-ai/savings-preview/releases/tags/${TAG}`) {
      const list = Object.keys(assets).map((name, i) => ({ name, url: `${api.url}/assets/${i}` }));
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ tag_name: TAG, assets: list }));
      return;
    }
    const m = req.url.match(/^\/assets\/(\d+)$/);
    if (m && req.headers.accept === 'application/octet-stream') {
      res.writeHead(302, { location: `${storage.url}/${Object.keys(assets)[Number(m[1])]}` }).end();
      return;
    }
    res.writeHead(404).end();
  });
  return {
    seen,
    opts: { api: api.url, web: web.url, plat: PLAT },
    close: () => [storage, web, api].forEach((s) => s.server.close()),
  };
}

function tmpdir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'npm-launcher-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test('maps Node platforms to release platforms', () => {
  assert.strictEqual(launcher.platform('darwin', 'arm64'), 'darwin_arm64');
  assert.strictEqual(launcher.platform('darwin', 'x64'), 'darwin_amd64');
  assert.strictEqual(launcher.platform('linux', 'x64'), 'linux_amd64');
  assert.strictEqual(launcher.platform('linux', 'arm64'), 'linux_arm64');
  assert.throws(() => launcher.platform('win32', 'x64'), /unsupported operating system/);
  assert.throws(() => launcher.platform('linux', 'ia32'), /unsupported architecture/);
});

test('reads one checksum per archive', () => {
  const a = 'a'.repeat(64), b = 'b'.repeat(64);
  assert.strictEqual(launcher.checksumFor(`${a}  x.tar.gz\n${b}  *y.tar.gz\n`, 'y.tar.gz'), b);
  assert.throws(() => launcher.checksumFor(`${a}  x.tar.gz\n`, 'y.tar.gz'), /no entry for y.tar.gz/);
  assert.throws(() => launcher.checksumFor(`${a}  x.tar.gz\n${b}  x.tar.gz\n`, 'x.tar.gz'), /more than one entry/);
});

test('puts the cache under the platform cache directory', () => {
  assert.strictEqual(launcher.cacheRoot({}, 'darwin', '/h'), '/h/Library/Caches/savings-preview');
  assert.strictEqual(launcher.cacheRoot({}, 'linux', '/h'), '/h/.cache/savings-preview');
  assert.strictEqual(launcher.cacheRoot({ XDG_CACHE_HOME: '/x' }, 'linux', '/h'), '/x/savings-preview');
  assert.strictEqual(launcher.cacheRoot({ SAVINGS_PREVIEW_CACHE: '/c' }, 'linux', '/h'), '/c');
});

test('downloads from the public URLs without a token, checks and caches the binary', async (t) => {
  const dir = tmpdir(t);
  const tgz = archive(dir);
  const r = await release({ [ARCHIVE]: tgz, 'checksums.txt': `${sha(tgz)}  ${ARCHIVE}\n` });
  t.after(r.close);
  const root = path.join(dir, 'cache');

  const bin = await launcher.ensureBinary('1.2.3', { ...r.opts, root, tok: '' });
  assert.strictEqual(bin, path.join(root, TAG, PLAT, 'savings-preview'));
  assert.strictEqual(execFileSync(bin, ['check', '--context', 'x'], { encoding: 'utf8' }), 'ran check --context x\n');
  assert.deepStrictEqual(r.seen.filter((s) => s.host === 'web').map((s) => s.url.split('/').pop()), [ARCHIVE, 'checksums.txt']);
  assert.ok(r.seen.every((s) => s.auth === undefined));
  assert.deepStrictEqual(fs.readdirSync(path.join(root, TAG)), [PLAT]);

  r.seen.length = 0;
  assert.strictEqual(await launcher.ensureBinary('1.2.3', { ...r.opts, root, tok: '' }), bin);
  assert.deepStrictEqual(r.seen, []);
});

test('downloads through the API with a token and never sends it to the storage host', async (t) => {
  const dir = tmpdir(t);
  const tgz = archive(dir);
  const r = await release({ [ARCHIVE]: tgz, 'checksums.txt': `${sha(tgz)}  ${ARCHIVE}\n` });
  t.after(r.close);

  const bin = await launcher.ensureBinary('1.2.3', { ...r.opts, root: path.join(dir, 'cache'), tok: 'secret' });
  assert.ok(fs.existsSync(bin));
  assert.ok(r.seen.filter((s) => s.host === 'api').every((s) => s.auth === 'Bearer secret'));
  const stored = r.seen.filter((s) => s.host === 'storage');
  assert.strictEqual(stored.length, 2);
  assert.ok(stored.every((s) => s.auth === undefined));
  assert.ok(!r.seen.some((s) => s.host === 'web'));
});

test('installs nothing when the checksum does not match', async (t) => {
  const dir = tmpdir(t);
  const tgz = archive(dir);
  const r = await release({ [ARCHIVE]: tgz, 'checksums.txt': `${'0'.repeat(64)}  ${ARCHIVE}\n` });
  t.after(r.close);
  const root = path.join(dir, 'cache');

  await assert.rejects(launcher.ensureBinary('1.2.3', { ...r.opts, root, tok: '' }), /checksum mismatch/);
  assert.deepStrictEqual(fs.readdirSync(path.join(root, TAG)), []);
});

test('installs nothing when checksums.txt does not list the archive', async (t) => {
  const dir = tmpdir(t);
  const tgz = archive(dir);
  const r = await release({ [ARCHIVE]: tgz, 'checksums.txt': `${sha(tgz)}  other.tar.gz\n` });
  t.after(r.close);

  await assert.rejects(launcher.ensureBinary('1.2.3', { ...r.opts, root: path.join(dir, 'cache'), tok: '' }), /no entry for/);
});

test('says how to authenticate when a release is not found without a token', async (t) => {
  const dir = tmpdir(t);
  const r = await release({});
  t.after(r.close);

  await assert.rejects(launcher.ensureBinary('1.2.3', { ...r.opts, root: path.join(dir, 'cache'), tok: '' }),
    /not found\. If the repository is private, log in with gh or set GITHUB_TOKEN/);
  await assert.rejects(launcher.ensureBinary('1.2.3', { ...r.opts, root: path.join(dir, 'cache'), tok: 'wrong' }),
    /not found, or the token cannot read it/);
});

test('the command runs the cached binary and passes on its exit code', (t) => {
  const dir = tmpdir(t);
  const plat = launcher.platform();
  const bin = path.join(dir, 'cache', 'v0.0.0', plat, 'savings-preview');
  fs.mkdirSync(path.dirname(bin), { recursive: true });
  fs.writeFileSync(bin, '#!/bin/sh\necho "ran $*"\nexit 3\n', { mode: 0o755 });

  const r = spawnSync(process.execPath, [path.join(__dirname, '..', 'packaging', 'npm', 'bin', 'savings-preview.js'), '--version'], {
    encoding: 'utf8',
    env: { ...process.env, SAVINGS_PREVIEW_CACHE: path.join(dir, 'cache') },
  });
  assert.strictEqual(r.stdout, 'ran --version\n');
  assert.strictEqual(r.status, 3);
});

test('reports a download that cannot connect as an error, not a crash', async () => {
  const { server, url } = await listen(() => {});
  server.close();
  await assert.rejects(launcher.fetchAssets(TAG, [ARCHIVE], { web: url, tok: '' }),
    (e) => e instanceof launcher.LaunchError && /^fetching http:\/\/127\.0\.0\.1:\d+\/.*: fetch failed \(ECONNREFUSED\)$/.test(e.message));
});

test('the command reports a cache it cannot write without a stack trace', (t) => {
  const dir = tmpdir(t);
  fs.writeFileSync(path.join(dir, 'file'), '');
  const r = spawnSync(process.execPath, [path.join(__dirname, '..', 'packaging', 'npm', 'bin', 'savings-preview.js'), '--version'], {
    encoding: 'utf8',
    env: { ...process.env, SAVINGS_PREVIEW_CACHE: path.join(dir, 'file', 'cache'), GITHUB_TOKEN: '', GH_TOKEN: '', PATH: path.dirname(process.execPath) },
  });
  assert.strictEqual(r.status, 1);
  assert.match(r.stderr, /^savings-preview: .*ENOTDIR/m);
  assert.doesNotMatch(r.stderr, /\n\s+at /);
});
