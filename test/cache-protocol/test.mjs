// Real @actions/cache -> executable -> disk -> archive extraction. Host records
// are fixtures; this does not pretend to boot a VM or authenticate with GitHub.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {mkdtemp, mkdir, writeFile, readFile, rename, rm, realpath} from 'node:fs/promises';
import {createServer} from 'node:net';
import {tmpdir} from 'node:os';
import {join, resolve} from 'node:path';
import {setTimeout as delay} from 'node:timers/promises';
import * as cache from '@actions/cache';

const binary = process.env.FIRERUNNER_BINARY;
assert(binary, 'Set FIRERUNNER_BINARY to the built firerunner executable');
const root = await realpath(await mkdtemp(join(tmpdir(), 'firerunner-cache-protocol-')));
const jobs = join(root, 'jobs');
const work = join(root, 'work');
const data = join(root, 'cache');
await mkdir(jobs, {mode: 0o700});
await mkdir(work, {mode: 0o700});
const probe = createServer();
probe.listen(0, '127.0.0.1');
await once(probe, 'listening');
const port = probe.address().port;
await new Promise(resolve => probe.close(resolve));
const base = `http://127.0.0.1:${port}`;
const child = spawn(resolve(binary), ['cache-server', '--addr', `127.0.0.1:${port}`,
  '--dir', data, '--repository', 'owner/repo', '--jobs-dir', jobs,
  '--default-ref', 'refs/heads/main', '--artifact-upstream', ''], {stdio: ['ignore', 'ignore', 'pipe']});
let logs = '';
child.stderr.on('data', chunk => { logs += chunk; });
const exited = once(child, 'exit');
const oldCwd = process.cwd();
process.chdir(work);
Object.assign(process.env, {
  ACTIONS_RESULTS_URL: `${base}/`, ACTIONS_CACHE_SERVICE_V2: 'true',
  ACTIONS_RUNTIME_TOKEN: 'deliberately-untrusted-fixture', ACTIONS_CACHE_MODE: 'write',
  GITHUB_WORKSPACE: work, RUNNER_TEMP: root
});
const main = 'refs/heads/main';
const branch = 'refs/heads/release';
const pr = 'refs/pull/7/merge';
let generation = 0;
async function identity(ref, reads = [ref, main]) {
  const job = `job-${++generation}`;
  const file = join(jobs, '127.0.0.1.json');
  await writeFile(`${file}.tmp`, JSON.stringify({
    ip: '127.0.0.1', runner: job, generation: job, repository: 'owner/repo',
    event: ref.startsWith('refs/pull/') ? 'pull_request' : 'push',
    write_ref: ref, read_refs: [...new Set(reads)], run_id: generation, job_id: job,
    expires_at: Math.floor(Date.now() / 1000) + 3600
  }), {mode: 0o600});
  await rename(`${file}.tmp`, file);
}
async function save(key, contents) {
  await mkdir('payload', {recursive: true});
  await writeFile('payload/value.txt', contents);
  assert(await cache.saveCache(['payload'], key) > 0, `save ${key} failed`);
}
async function restore(key, expected, restoreKeys = [], matched = key) {
  await rm('payload', {recursive: true, force: true});
  const actual = await cache.restoreCache(['payload'], key, restoreKeys);
  if (expected === undefined) {
    assert.equal(actual, undefined, `unexpected cache hit for ${key}`);
  } else {
    assert.equal(actual, matched);
    assert.equal(await readFile('payload/value.txt', 'utf8'), expected);
  }
}
try {
  let ready = false;
  for (let i = 0; i < 100; i++) {
    if (child.exitCode !== null) throw new Error(`server exited: ${logs}`);
    try { ready = (await fetch(`${base}/stats`)).ok; } catch {}
    if (ready) break;
    await delay(100);
  }
  assert(ready, `server never became ready: ${logs}`);
  await identity(main);
  await save('shared', 'DEFAULT');
  await save('dependencies-new', 'DEFAULT EXACT');
  await identity(branch);
  await restore('shared', 'DEFAULT');
  await save('shared', 'BASE');
  await identity(pr, [pr, branch, main]);
  await restore('shared', 'BASE');
  await save('shared', 'PR');
  await save('pr-only', 'PR PRIVATE');
  await save('dependencies-old', 'OWN PREFIX');
  await restore('dependencies-new', 'OWN PREFIX', ['dependencies-'], 'dependencies-old');
  // Re-run of the same PR can restore its entries with a new job identity.
  await identity(pr, [pr, branch, main]);
  await restore('shared', 'PR');
  await identity('refs/pull/8/merge', ['refs/pull/8/merge', branch, main]);
  await restore('shared', 'BASE');
  await restore('pr-only', undefined);
  await identity(main);
  await restore('shared', 'DEFAULT');
  await restore('pr-only', undefined);
  // Before JobStarted, only the configured default scope can be read.
  await rm(join(jobs, '127.0.0.1.json'));
  await restore('shared', 'DEFAULT');
  const saved = await cache.saveCache(['payload'], 'unknown-job');
  assert.equal(saved, -1, 'unknown job acquired a cache reservation');
  console.log('PASS: actual toolkit save/extract, ref precedence, isolation, rerun, default-only fallback, denied writes');
} catch (error) {
  console.error(logs);
  throw error;
} finally {
  child.kill('SIGTERM');
  await exited;
  process.chdir(oldCwd);
  await rm(root, {recursive: true, force: true});
}
