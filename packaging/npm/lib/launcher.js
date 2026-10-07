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
  api: 'https://api.github.com',
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

// token returns GITHUB_TOKEN, GH_TOKEN or the token of a logged-in gh, in that
// order, or an empty string.
function token(env = process.env) {
  if (env.GITHUB_TOKEN) return env.GITHUB_TOKEN;
  if (env.GH_TOKEN) return env.GH_TOKEN;
  try {
    const r = spawnSync('gh', ['auth', 'token', '-h', 'github.com'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    if (r.status === 0) return r.stdout.trim();
  } catch {
    // gh is not installed.
  }
  return '';
}

// get fetches url and follows redirects itself, so the token never goes to the
// storage host a release asset redirects to.
async function get(url, headers) {
  for (let i = 0; i < 5; i++) {
    const res = await fetch(url, { headers, redirect: 'manual' });
    if (res.status >= 300 && res.status < 400 && res.headers.get('location')) {
      const next = new URL(res.headers.get('location'), url);
      if (next.origin !== new URL(url).origin) {
        headers = Object.fromEntries(Object.entries(headers).filter(([k]) => k.toLowerCase() !== 'authorization'));
      }
      url = next.href;
      continue;
    }
    return res;
  }
  throw new LaunchError(`too many redirects fetching ${url}`);
}

// fetchAssets downloads the named release assets. Without a token they come
// from the public download URLs; with one, through the GitHub API, which also
// serves private repositories.
async function fetchAssets(tag, names, { tok, api = DEFAULTS.api, web = DEFAULTS.web } = {}) {
  const out = {};
  if (!tok) {
    for (const name of names) {
      const url = `${web}/${REPO}/releases/download/${tag}/${name}`;
      const res = await get(url, {});
      if (res.status === 404) {
        throw new LaunchError(
          `release ${tag} of ${REPO} or its asset ${name} was not found. If the repository is private, ` +
          'log in with gh or set GITHUB_TOKEN to a token that can read it.');
      }
      if (!res.ok) throw new LaunchError(`downloading ${name} from release ${tag}: HTTP ${res.status}`);
      out[name] = Buffer.from(await res.arrayBuffer());
    }
    return out;
  }
  const headers = { Authorization: `Bearer ${tok}`, 'X-GitHub-Api-Version': '2022-11-28' };
  const rel = await get(`${api}/repos/${REPO}/releases/tags/${tag}`, { ...headers, Accept: 'application/vnd.github+json' });
  if (rel.status === 404) throw new LaunchError(`release ${tag} of ${REPO} was not found, or the token cannot read it`);
  if (!rel.ok) throw new LaunchError(`reading release ${tag} of ${REPO}: HTTP ${rel.status}`);
  const assets = (await rel.json()).assets || [];
  for (const name of names) {
    const asset = assets.find((a) => a.name === name);
    if (!asset) throw new LaunchError(`release ${tag} of ${REPO} has no asset named ${name}`);
    const res = await get(asset.url, { ...headers, Accept: 'application/octet-stream' });
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

  const archive = archiveName(tag, plat);
  log(`Downloading ${BINARY} ${tag} for ${plat}`);
  const tok = 'tok' in opts ? opts.tok : token();
  const files = await fetchAssets(tag, [archive, 'checksums.txt'], { ...opts, tok });
  const want = checksumFor(files['checksums.txt'].toString('utf8'), archive);
  const got = crypto.createHash('sha256').update(files[archive]).digest('hex');
  if (got !== want) throw new LaunchError(`checksum mismatch for ${archive}: expected ${want}, got ${got}`);
  log('Checksum verified');

  fs.mkdirSync(path.dirname(dir), { recursive: true });
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
    if (!(e instanceof LaunchError)) throw e;
    process.stderr.write(`savings-preview: ${e.message}\n`);
    process.exit(1);
  }
}

module.exports = { LaunchError, archiveName, cacheRoot, checksumFor, ensureBinary, fetchAssets, main, platform, token };
