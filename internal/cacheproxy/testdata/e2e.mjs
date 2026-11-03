// End-to-end check of the truner cache proxy with the real @actions/cache
// toolkit. Run through TestNodeToolkitE2E.
import * as cache from '@actions/cache';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

const work = process.argv[2];
fs.mkdirSync(process.env.RUNNER_TEMP, { recursive: true });

function makeData(dir, mb) {
  fs.rmSync(dir, { recursive: true, force: true });
  fs.mkdirSync(dir, { recursive: true });
  const h = crypto.createHash('sha256');
  // Random data does not compress, so the archive stays large enough to
  // exercise the Azure staged-block upload path (> 128 MiB single shot).
  for (let i = 0; i < mb; i++) {
    const buf = crypto.randomBytes(1 << 20);
    fs.writeFileSync(path.join(dir, `f${i}.bin`), buf);
    h.update(buf);
  }
  return h.digest('hex');
}

function hashDir(dir, mb) {
  const h = crypto.createHash('sha256');
  for (let i = 0; i < mb; i++) h.update(fs.readFileSync(path.join(dir, `f${i}.bin`)));
  return h.digest('hex');
}

async function roundTrip(name, mb) {
  const dir = path.join(work, name);
  const want = makeData(dir, mb);
  const key = `${name}-${Date.now()}`;
  let t = Date.now();
  const id = await cache.saveCache([dir], key);
  console.log(`[${name}] saved ${mb} MiB as id ${id} in ${Date.now() - t} ms`);
  fs.rmSync(dir, { recursive: true, force: true });
  t = Date.now();
  const hit = await cache.restoreCache([dir], `${name}-missing`, [`${name}-`]);
  console.log(`[${name}] restored key ${hit} in ${Date.now() - t} ms`);
  if (hit !== key) throw new Error(`expected restore-key hit ${key}, got ${hit}`);
  const got = hashDir(dir, mb);
  if (got !== want) throw new Error(`content mismatch for ${name}`);
  const exact = await cache.restoreCache([dir], key, [], { lookupOnly: true });
  if (exact !== key) throw new Error(`exact lookup failed: ${exact}`);
  console.log(`[${name}] OK`);
}

// The proxy refuses a taken key with HTTP 409 like GitHub does. The toolkit
// must only log that and return -1, never fail the step.
async function saveExisting() {
  const dir = path.join(work, 'dup');
  makeData(dir, 1);
  const key = `dup-${Date.now()}`;
  const id = await cache.saveCache([dir], key);
  if (id <= 0) throw new Error(`first save of ${key} failed: ${id}`);
  const again = await cache.saveCache([dir], key);
  if (again !== -1) throw new Error(`expected -1 when saving existing key ${key}, got ${again}`);
  console.log('[dup] OK');
}

await roundTrip('small', 3);
await roundTrip('large', 200);
await saveExisting();
console.log('ALL OK');
