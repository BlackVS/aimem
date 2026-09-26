#!/usr/bin/env node
// End-to-end check of the OpenCode plugin (.opencode/plugin/aimem.ts)
// against a real OpenCode executable, 1.x or 2.x.
//
// Usage: node scripts/opencode-plugin-e2e/run.cjs /path/to/opencode [scenario...]
// Scenarios: text tool fail warn compact (default: all that apply to
// the detected generation). Linux/macOS only: the fake `aimem` that
// records submits is a shell script.
//
// Each scenario runs `opencode run` once in a disposable project and HOME,
// against a local scripted OpenAI-compatible provider (no paid model, no
// network). The project carries the plugin and a fake `aimem` binary that
// appends every `aimem submit` payload to a log; the scenario then asserts
// on those payloads and on what reached the model.
'use strict';
const assert = require('node:assert/strict');
const cp = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');

const repo = path.resolve(__dirname, '..', '..');
const exe = process.argv[2];
if (!exe || !fs.existsSync(exe)) {
  console.error('usage: node scripts/opencode-plugin-e2e/run.cjs /path/to/opencode [scenario...]');
  process.exit(2);
}
if (process.platform === 'win32') {
  console.error('the fake aimem is a shell script; run this on Linux or macOS');
  process.exit(2);
}
const version = cp.execFileSync(exe, ['--version'], { encoding: 'utf8', env: { ...process.env, HOME: os.tmpdir() } }).trim().replace(/^opencode\s+/i, '');
const major = Number((version.match(/(\d+)\.\d+/) || [])[1]);
const v2 = major >= 2;

// Scenarios. `v2only` marks behavior that exists only on OpenCode 2 (the
// model-facing context warning) or whose trigger point differs by
// generation (auto-compaction thresholds).
const scenarios = {
  text: { mode: 'text' },
  tool: { mode: 'tool' },
  // A failed turn may end `opencode run` with 0 or 1 depending on the
  // release; either is a normal exit (a timeout or signal is not).
  fail: { mode: 'fail', failExit: [0, 1] },
  warn: { mode: 'tool', limit: 100000, first: 60000, env: { AIMEM_CTX_WARN_FRACTION: '0.5' }, v2only: true },
  compact: { mode: 'tool', limit: 100000, first: 90000, v2only: true },
  // Long-lived server scenarios (2.x `opencode serve`, driven over its
  // HTTP API): several turns in one plugin process.
  // queued: B is queued behind a slow A and cancelled before delivery; A
  // must keep its own request and B must not be journaled.
  queued: { mode: 'text', delayFirstMs: 6000, serve: 'queued', v2only: true },
  // second: turn 1 succeeds, turn 2 fails; the failure must be journaled
  // under its own turn, not dropped as a repeat of turn 1.
  second: { mode: 'text', failFrom: 2, serve: 'second', v2only: true },
};

// A scripted provider: `text` answers, `tool` calls glob once then
// answers, `fail` rejects agent requests. `failFrom` rejects agent
// requests from that one on (1-based); `delayFirstMs` holds the first
// agent answer back. Compaction requests get a summary in OpenCode 2's
// required template.
function provider(mode, firstTokens, opts = {}) {
  const requests = [];
  let primary = 0;
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', c => { raw += c; });
    req.on('end', () => {
      if (req.method === 'GET') {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ object: 'list', data: [{ id: 'mock', object: 'model' }] }));
        return;
      }
      const body = JSON.parse(raw || '{}');
      const text = JSON.stringify(body);
      const messages = body.messages || [];
      const compaction = text.includes('You MUST use this format');
      const isPrimary = Array.isArray(body.tools) && body.tools.length > 0 && !compaction;
      if (isPrimary) primary += 1;
      const index = isPrimary ? primary : 0;
      requests.push({
        compaction, primary: isPrimary,
        handoff: text.includes('MARKER-HANDOFF'),
        handoffNote: text.includes('AIMEM HANDOFF'),
        warning: text.includes("aimem: this session's context"),
      });
      if (isPrimary && ((mode === 'fail') || (opts.failFrom && index >= opts.failFrom))) {
        res.writeHead(400, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: { message: 'scripted failure', type: 'invalid_request_error' } }));
        return;
      }
      if (!body.stream) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ id: 'c', object: 'chat.completion', created: 0, model: 'mock',
          choices: [{ index: 0, message: { role: 'assistant', content: 'Mock title' }, finish_reason: 'stop' }],
          usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }));
        return;
      }
      let tokens = 100;
      let call = null;
      let reply = 'MOCK-REPLY-OK';
      if (compaction) reply = '## Objective\nScripted objective.\n\n## Next Move\nContinue.';
      else if (isPrimary) {
        if (index === 1) tokens = firstTokens;
        const last = messages[messages.length - 1];
        if (mode === 'tool' && !(last && last.role === 'tool')) call = { name: 'glob', arguments: JSON.stringify({ pattern: '*.md' }) };
      }
      const answer = () => {
      const base = { id: 'c', object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000), model: 'mock' };
      const usage = { prompt_tokens: tokens, completion_tokens: 10, total_tokens: tokens + 10 };
      const send = d => res.write('data: ' + JSON.stringify(d) + '\n\n');
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      if (call) {
        send({ ...base, choices: [{ index: 0, delta: { role: 'assistant', tool_calls: [{ index: 0, id: 'call_1', type: 'function', function: call }] }, finish_reason: null }] });
        send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }], usage });
      } else {
        send({ ...base, choices: [{ index: 0, delta: { role: 'assistant', content: reply }, finish_reason: null }] });
        send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }], usage });
      }
      res.end('data: [DONE]\n\n');
      };
      if (index === 1 && opts.delayFirstMs) setTimeout(answer, opts.delayFirstMs);
      else answer();
    });
  });
  return new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve({ server, requests, port: server.address().port })));
}

// serveRun drives a 2.x `opencode serve` over its HTTP API, so several
// turns reach one plugin process. It returns the same shape as a CLI run:
// status 0 when every step completed and the server stayed up until told
// to stop.
async function serveRun(proj, env, script) {
  const port = await new Promise(resolve => {
    const l = http.createServer().listen(0, '127.0.0.1', () => { const n = l.address().port; l.close(() => resolve(n)); });
  });
  const password = crypto.randomBytes(12).toString('hex');
  const child = cp.spawn(exe, ['serve', '--hostname', '127.0.0.1', '--port', String(port)],
    { cwd: proj, env: { ...env, OPENCODE_PASSWORD: password }, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  let exited = null;
  child.stderr.on('data', c => { stderr = (stderr + c).slice(-4000); });
  const closed = new Promise(resolve => child.on('close', (status, signal) => { exited = { status, signal }; resolve(); }));
  const auth = 'Basic ' + Buffer.from('opencode:' + password).toString('base64');
  const timeout = Number(process.env.AIMEM_E2E_TIMEOUT_MS) || 150000;
  const deadline = Date.now() + timeout;
  const api = async (method, route, body) => {
    const res = await fetch(`http://127.0.0.1:${port}${route}`, {
      method, headers: { authorization: auth, 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(Math.max(1000, deadline - Date.now())),
    });
    if (!res.ok) throw new Error(`${method} ${route}: HTTP ${res.status}`);
    if (res.status === 204) return null;
    const json = await res.json();
    return json && json.data !== undefined ? json.data : json;
  };
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
  let failure = null;
  try {
    for (;;) {
      if (exited) throw new Error('opencode serve exited before it was ready');
      if (Date.now() > deadline) throw new Error('opencode serve never became ready');
      try { await api('GET', '/api/session'); break; } catch { await sleep(500); }
    }
    const session = await api('POST', '/api/session', { location: { directory: proj } });
    const sid = session.id;
    const prompt = (text, delivery) => api('POST', `/api/session/${sid}/prompt`, { text, ...(delivery ? { delivery } : {}) });
    const wait = () => api('POST', `/api/experimental/session/${sid}/wait`);
    if (script === 'queued') {
      await prompt('REQUEST-A');
      await sleep(1500); // A is now held by the provider
      const b = await prompt('REQUEST-B', 'queue');
      await api('DELETE', `/api/session/${sid}/inbox/${b.id}`);
      await wait();
    } else if (script === 'second') {
      await prompt('REQUEST-1');
      await wait();
      await prompt('REQUEST-2');
      await wait();
    }
    if (exited) throw new Error(`opencode serve exited early (${exited.status ?? exited.signal})`);
  } catch (error) {
    failure = error;
  }
  const timedOut = Date.now() > deadline;
  child.kill('SIGTERM');
  const killer = setTimeout(() => child.kill('SIGKILL'), 10000);
  await closed;
  clearTimeout(killer);
  return { status: failure ? 1 : 0, signal: null, timedOut, stderr, error: failure && failure.message };
}

function readSubmits(file) {
  if (!fs.existsSync(file)) return [];
  // Concurrent submits may land on one line; split on record boundaries.
  return fs.readFileSync(file, 'utf8').split(/\n|(?<=\})(?=\{"project_dir")/)
    .filter(l => l.startsWith('{')).map(l => JSON.parse(l));
}

async function run(name, sc) {
  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'aimem-oc-e2e-'));
  const home = path.join(work, 'home');
  const proj = path.join(work, 'proj');
  const log = path.join(work, 'submits.log');
  fs.mkdirSync(path.join(proj, '.opencode', 'plugin'), { recursive: true });
  fs.mkdirSync(path.join(proj, 'docs'), { recursive: true });
  fs.mkdirSync(home, { recursive: true });
  fs.copyFileSync(path.join(repo, '.opencode', 'plugin', 'aimem.ts'), path.join(proj, '.opencode', 'plugin', 'aimem.ts'));
  fs.writeFileSync(path.join(proj, 'aimem'), '#!/bin/sh\n[ "$1" = submit ] && { cat; echo; } >> "$AIMEM_E2E_LOG"\nexit 0\n', { mode: 0o755 });
  fs.writeFileSync(path.join(proj, 'docs', 'SESSION-STATE.md'), '# Handoff\n\nMARKER-HANDOFF\n');
  const p = await provider(sc.mode, sc.first || 100, { delayFirstMs: sc.delayFirstMs, failFrom: sc.failFrom });
  fs.writeFileSync(path.join(proj, 'opencode.json'), JSON.stringify({
    $schema: 'https://opencode.ai/config.json',
    instructions: ['docs/SESSION-STATE.md'],
    model: 'mock/mock', small_model: 'mock/mock',
    provider: { mock: { npm: '@ai-sdk/openai-compatible', name: 'Mock',
      options: { baseURL: `http://127.0.0.1:${p.port}/v1`, apiKey: 'disposable-not-a-secret' },
      models: { mock: { name: 'mock', limit: { context: sc.limit || 200000, output: 1000 } } } } },
  }, null, 2));
  cp.execFileSync('git', ['init', '-q', proj]);
  const env = {
    PATH: process.env.PATH, HOME: home, TERM: 'dumb',
    XDG_CONFIG_HOME: path.join(home, '.config'), XDG_DATA_HOME: path.join(home, '.local', 'share'),
    XDG_CACHE_HOME: path.join(home, '.cache'), XDG_STATE_HOME: path.join(home, '.local', 'state'),
    OPENCODE_DISABLE_AUTOUPDATE: '1', NO_PROXY: '127.0.0.1,localhost', AIMEM_E2E_LOG: log,
    ...(process.env.HTTPS_PROXY ? { HTTPS_PROXY: process.env.HTTPS_PROXY } : {}),
    ...(process.env.NODE_EXTRA_CA_CERTS ? { NODE_EXTRA_CA_CERTS: process.env.NODE_EXTRA_CA_CERTS } : {}),
    ...(sc.env || {}),
  };
  const args = ['run', ...(v2 ? ['--standalone', '--auto'] : []), 'say hi please'];
  // Async on purpose: the scripted provider runs in this process, so a
  // blocking spawn would starve it.
  const r = sc.serve ? await serveRun(proj, env, sc.serve) : await new Promise(resolve => {
    const child = cp.spawn(exe, args, { cwd: proj, env, stdio: ['ignore', 'ignore', 'pipe'] });
    let stderr = '';
    child.stderr.on('data', c => { stderr = (stderr + c).slice(-4000); });
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; child.kill('SIGKILL'); }, Number(process.env.AIMEM_E2E_TIMEOUT_MS) || 150000);
    child.on('close', (status, signal) => { clearTimeout(timer); resolve({ status, signal, timedOut, stderr }); });
  });
  // Submits are detached; give the last one a moment to land.
  await new Promise(resolve => setTimeout(resolve, 1500));
  p.server.close();
  const records = readSubmits(log);
  const submits = records.map(r => r.event);
  const turns = submits.filter(e => e.kind === 'turn');
  const failures = submits.filter(e => e.kind === 'failure');
  const markers = submits.filter(e => e.kind === 'compaction-marker');
  const primaries = p.requests.filter(x => x.primary);
  try {
    // Process health first: correct payloads from a run that then hung or
    // crashed must not pass.
    assert.ok(!r.timedOut, 'opencode run timed out');
    if (r.error) assert.fail(`opencode serve: ${r.error}`);
    assert.equal(r.signal, null, `opencode run was killed by ${r.signal}`);
    if (name === 'fail') assert.ok(sc.failExit.includes(r.status), `unexpected exit ${r.status} for a failed turn`);
    else assert.equal(r.status, 0, `opencode run exited ${r.status}`);
    assert.ok(p.requests.length > 0, 'the provider was never called');
    assert.ok(primaries.some(x => x.handoff), 'docs/SESSION-STATE.md never reached the model');
    for (const rec of records) {
      assert.equal(fs.realpathSync(rec.project_dir), fs.realpathSync(proj), 'submit names the wrong project');
      assert.equal(rec.event.client, 'opencode');
      assert.ok(rec.event.idempotency_key.startsWith(`opencode:${rec.event.session_id}:`));
    }
    switch (name) {
      case 'text':
        assert.equal(turns.length, 1, 'expected exactly one journaled turn');
        assert.equal(turns[0].assistant_response, 'MOCK-REPLY-OK');
        assert.match(turns[0].user_request, /say hi please/);
        break;
      case 'tool':
        assert.equal(turns.length, 1, 'expected exactly one journaled turn');
        assert.deepEqual(turns[0].tool_summary, ['glob']);
        break;
      case 'fail':
        assert.ok(failures.length >= 1, 'expected a failure event');
        // 1.x may also report the same turn as ok; one idempotency key.
        assert.equal(new Set(submits.map(e => e.idempotency_key)).size, 1);
        break;
      case 'warn':
        assert.ok(primaries.some(x => x.warning), 'the context warning never reached the model');
        break;
      case 'queued':
        assert.equal(turns.length, 1, 'expected exactly one journaled turn');
        assert.equal(turns[0].user_request, 'REQUEST-A', 'the running turn lost its own request');
        assert.equal(turns[0].assistant_response, 'MOCK-REPLY-OK');
        assert.ok(!JSON.stringify(submits).includes('REQUEST-B'), 'the cancelled prompt was journaled');
        break;
      case 'second':
        assert.equal(turns.length, 1, 'expected one successful turn');
        assert.equal(turns[0].user_request, 'REQUEST-1');
        assert.equal(failures.length, 1, 'the second turn\'s failure was not journaled');
        assert.equal(failures[0].user_request, 'REQUEST-2');
        assert.notEqual(failures[0].idempotency_key, turns[0].idempotency_key);
        break;
      case 'compact':
        assert.ok(p.requests.some(x => x.compaction && x.handoffNote), 'compaction request lacks the AIMEM HANDOFF note');
        assert.equal(markers.length, 1, 'expected one compaction marker');
        break;
    }
    fs.rmSync(work, { recursive: true, force: true });
    return { name, ok: true, exit: r.status, submits: submits.map(e => `${e.kind}/${e.outcome}`) };
  } catch (error) {
    return { name, ok: false, error: error.message, exit: r.status, work, stderr: (r.stderr || '').slice(-2000) };
  }
}

(async () => {
  const wanted = process.argv.slice(3);
  const names = (wanted.length ? wanted : Object.keys(scenarios)).filter(n => {
    if (!scenarios[n]) throw new Error('unknown scenario ' + n);
    return v2 || !scenarios[n].v2only || wanted.includes(n);
  });
  console.log(`OpenCode ${version} (${v2 ? '2.x plugin path' : '1.x plugin path'})`);
  let failed = 0;
  for (const n of names) {
    const res = await run(n, scenarios[n]);
    if (res.ok) console.log(`  ok    ${n}  exit=${res.exit}  ${res.submits.join(' ')}`);
    else {
      failed += 1;
      console.log(`  FAIL  ${n}  ${res.error} (exit ${res.exit}; kept ${res.work})`);
      if (process.env.AIMEM_E2E_VERBOSE) console.log(res.stderr);
    }
  }
  process.exit(failed ? 1 : 0);
})().catch(error => { console.error(error); process.exit(1); });
