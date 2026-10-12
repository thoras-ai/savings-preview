'use strict';
// Downloads the savings-preview release binary that matches this package's
// version, checks it against the release's checksums.txt, keeps it in the user
// cache directory and runs it with the given arguments.

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

const REPO = 'thoras-ai/savings-preview';
const BINARY = 'savings-preview';
const DEFAULTS = {
  web: 'https://github.com',
};

class LaunchError extends Error {}

function platform(p = process.platform, a = process.arch) {
  const goos = { darwin: 'darwin', linux: 'linux' }[p];
  const goarch = { x64: 'amd64', arm64: 'arm64' }[a];
  if (!goos) throw new LaunchError(`unsupported operating system: ${p} (supported: macOS and Linux)`);
  if (!goarch) throw new LaunchError(`unsupported architecture: ${a} (supported: x64 and arm64)`);
  return `${goos}_${goarch}`;
}

function archiveName(tag, plat) {
  return `${BINARY}_${tag}_${plat}.tar.gz`;
}

// checksumFor returns the SHA-256 that checksums.txt lists for name.
function checksumFor(text, name) {
  const found = [];
  for (const line of text.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length === 2 && parts[1].replace(/^\*/, '') === name) found.push(parts[0]);
  }
  if (found.length === 0) throw new LaunchError(`checksums.txt has no entry for ${name}`);
  if (found.length > 1) throw new LaunchError(`checksums.txt has more than one entry for ${name}`);
  if (!/^[0-9a-f]{64}$/.test(found[0])) throw new LaunchError(`checksums.txt has a malformed entry for ${name}`);
  return found[0];
}

function cacheRoot(env = process.env, p = process.platform, home = os.homedir()) {
  if (env.SAVINGS_PREVIEW_CACHE) return env.SAVINGS_PREVIEW_CACHE;
  if (p === 'darwin') return path.join(home, 'Library', 'Caches', BINARY);
  return path.join(env.XDG_CACHE_HOME || path.join(home, '.cache'), BINARY);
}

async function get(url) {
  try {
    return await fetch(url);
  } catch (e) {
    // fetch reports DNS, connection and TLS failures as "fetch failed" with the reason in cause.
    const cause = e.cause && (e.cause.code || e.cause.message);
    throw new LaunchError(`fetching ${url}: ${e.message}${cause ? ` (${cause})` : ''}`);
  }
}

// fetchAssets downloads the named assets from the release's public download URLs.
async function fetchAssets(tag, names, { web = DEFAULTS.web } = {}) {
  const out = {};
  for (const name of names) {
    const res = await get(`${web}/${REPO}/releases/download/${tag}/${name}`);
    if (res.status === 404) throw new LaunchError(`release ${tag} of ${REPO} has no asset named ${name}`);
    if (!res.ok) throw new LaunchError(`downloading ${name} from release ${tag}: HTTP ${res.status}`);
    out[name] = Buffer.from(await res.arrayBuffer());
  }
  return out;
}

// ensureBinary returns the path of the cached binary for version, downloading
// and checking it first when it is not there.
async function ensureBinary(version, { root = cacheRoot(), plat = platform(), log = () => {}, ...opts } = {}) {
  const tag = `v${version}`;
  const dir = path.join(root, tag, plat);
  const bin = path.join(dir, BINARY);
  if (fs.existsSync(bin)) return bin;

  fs.mkdirSync(path.dirname(dir), { recursive: true });
  const archive = archiveName(tag, plat);
  log(`Downloading ${BINARY} ${tag} for ${plat}`);
  const files = await fetchAssets(tag, [archive, 'checksums.txt'], opts);
  const want = checksumFor(files['checksums.txt'].toString('utf8'), archive);
  const got = crypto.createHash('sha256').update(files[archive]).digest('hex');
  if (got !== want) throw new LaunchError(`checksum mismatch for ${archive}: expected ${want}, got ${got}`);
  log('Checksum verified');

  const tmp = fs.mkdtempSync(path.join(path.dirname(dir), `.${plat}-`));
  try {
    fs.writeFileSync(path.join(tmp, archive), files[archive]);
    const r = spawnSync('tar', ['-xzf', archive], { cwd: tmp, stdio: ['ignore', 'ignore', 'pipe'], encoding: 'utf8' });
    if (r.error) throw new LaunchError(`running tar: ${r.error.message}`);
    if (r.status !== 0) throw new LaunchError(`unpacking ${archive}: ${r.stderr.trim()}`);
    const unpacked = path.join(tmp, `${BINARY}_${tag}_${plat}`, BINARY);
    if (!fs.existsSync(unpacked)) throw new LaunchError(`${archive} does not contain ${BINARY}_${tag}_${plat}/${BINARY}`);
    const staged = path.join(tmp, 'out');
    fs.mkdirSync(staged);
    fs.renameSync(unpacked, path.join(staged, BINARY));
    fs.chmodSync(path.join(staged, BINARY), 0o755);
    try {
      fs.renameSync(staged, dir);
    } catch (e) {
      // Another run unpacked the same release first.
      if (!fs.existsSync(bin)) throw e;
    }
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  return bin;
}

function run(bin, args) {
  const child = spawn(bin, args, { stdio: 'inherit' });
  // The terminal sends Ctrl-C to the binary as well; it decides when to exit.
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.on(sig, () => { if (sig !== 'SIGINT') child.kill(sig); });
  }
  child.on('error', (e) => {
    process.stderr.write(`savings-preview: running ${bin}: ${e.message}\n`);
    process.exit(1);
  });
  child.on('exit', (code, signal) => {
    if (signal) {
      process.removeAllListeners(signal);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code);
  });
}

async function main(args) {
  const { version } = require('../package.json');
  try {
    const bin = await ensureBinary(version, { log: (m) => process.stderr.write(`${m}\n`) });
    run(bin, args);
  } catch (e) {
    process.stderr.write(`savings-preview: ${e.message}\n`);
    process.exit(1);
  }
}

module.exports = { LaunchError, archiveName, cacheRoot, checksumFor, ensureBinary, fetchAssets, main, platform };
