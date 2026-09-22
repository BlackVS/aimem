// Usage: node scripts/agent-probe/run.cjs ABSOLUTE_CODEX_EXE ABSOLUTE_OPENCODE_EXE
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const readline = require('node:readline');
const net = require('node:net');
const crypto = require('node:crypto');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aimem-agent-probe-'));
const work = path.join(root, 'workspace');
fs.mkdirSync(work);
const env = {};
for (const k of ['PATH', 'Path', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATHEXT', 'TEMP', 'TMP']) {
  if (process.env[k]) env[k] = process.env[k];
}
for (const k of ['HOME', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'XDG_CONFIG_HOME',
  'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_STATE_HOME', 'CODEX_HOME']) {
  env[k] = path.join(root, k);
  fs.mkdirSync(env[k]);
}
const children = [];
const report = { platform: process.platform, node: process.version, provider: 'scripted loopback',
  paid_model_turns: 0, observations: [] };
let provider;
function observe(client, check, value) { report.observations.push({ client, check, ...value }); }
function child(exe, args, extra = {}) {
  const p = spawn(exe, args, { cwd: work, env: { ...env, ...extra },
    windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
  children.push(p);
  const diagnostic = path.join(root, 'child-' + children.length + '.stderr');
  p.stderr.on('data', chunk => fs.appendFileSync(diagnostic, chunk));
  p.on('error', error => { p.probeError = error; });
  return p;
}
async function stop(p) {
  if (p.exitCode !== null || p.signalCode !== null || p.probeError) return;
  p.kill();
  await until(() => p.exitCode !== null || p.signalCode !== null, 'child exit');
}
async function until(predicate, label, milliseconds = 20000) {
  const deadline = Date.now() + milliseconds;
  while (Date.now() < deadline) {
    const value = await predicate();
    if (value) return value;
    await delay(50);
  }
  throw Error('Timeout: ' + label);
}
async function version(exe) {
  assert(path.isAbsolute(exe), 'Pass absolute executable paths, not launchers');
  const p = child(exe, ['--version']);
  let output = '';
  p.stdout.on('data', chunk => { output += chunk; });
  await until(() => p.probeError || p.exitCode !== null, 'version');
  if (p.probeError) throw p.probeError;
  assert.equal(p.exitCode, 0);
  return output.trim();
}
function rpc(p) {
  let id = 0;
  const pending = new Map(), events = [];
  readline.createInterface({ input: p.stdout }).on('line', line => {
    let value;
    try { value = JSON.parse(line); } catch { return; }
    if (pending.has(value.id)) {
      pending.get(value.id)(value);
      pending.delete(value.id);
    } else events.push(value);
  });
  return {
    events,
    send(method, params = {}) {
      return new Promise((resolve, reject) => {
        const n = ++id;
        const timer = setTimeout(() => { pending.delete(n); reject(Error('RPC timeout: ' + method)); }, 20000);
        pending.set(n, value => { clearTimeout(timer); resolve(value); });
        p.stdin.write(JSON.stringify({ id: n, method, params }) + '\n');
      });
    },
    notify(method) { p.stdin.write(JSON.stringify({ method }) + '\n'); },
  };
}
function result(response) { assert(!response.error, JSON.stringify(response.error)); return response.result; }
const fixtureFile = name => path.join(root, name + '-fixture.json');
function checkFixture(client) {
  const f = JSON.parse(fs.readFileSync(fixtureFile(client), 'utf8'));
  assert(f.joined && f.answered && f.acked);
  assert.deepEqual(f.calls.map(c => c.name), ['join', 'inbox', 'inbox', 'reply', 'ack', 'inbox'].map(n => 'probe_' + n));
  assert.equal(f.calls[1].result.messages[0].id, f.calls[2].result.messages[0].id);
  assert.equal(f.calls[3].result.in_reply_to, 'fixture-message');
  assert.equal(f.calls[5].args.cursor, 1);
  assert.deepEqual(f.calls[5].result.messages, []);
  observe(client, 'MCP_sequence', { pass: true, protocol: f.protocol, calls: f.calls });
}
async function idleNotification(client) {
  const before = provider.log.length;
  fs.writeFileSync(fixtureFile(client) + '.notify', 'fixture');
  await until(() => {
    try { return JSON.parse(fs.readFileSync(fixtureFile(client), 'utf8')).notifications === 1; }
    catch { return false; }
  },
    'notification emitted');
  await delay(2000);
  observe(client, 'idle_logging_notification', { window_ms: 2000,
    emitted: true, additional_provider_requests: provider.log.length - before });
}
async function codex(exe) {
  const config = `model = "fixture"\nmodel_provider = "fixture"\n[model_providers.fixture]\n`
    + `name = "fixture"\nbase_url = ${JSON.stringify(provider.base)}\nwire_api = "responses"\nrequires_openai_auth = false\n`
    + `[mcp_servers.fixture]\ncommand = ${JSON.stringify(process.execPath)}\n`
    + `args = ${JSON.stringify([path.join(__dirname, 'mcp.cjs'), fixtureFile('codex')])}\n`
    + ['join', 'inbox', 'reply', 'ack'].map(n => `[mcp_servers.fixture.tools.probe_${n}]\napproval_mode = "approve"\n`).join('');
  fs.writeFileSync(path.join(env.CODEX_HOME, 'config.toml'), config);
  const p = child(exe, ['app-server', '--stdio']), r = rpc(p);
  assert.equal((await r.send('thread/loaded/list')).error?.code, -32600);
  result(await r.send('initialize', { clientInfo: { name: 'aimem_probe', version: '1' } }));
  r.notify('initialized');
  const thread = result(await r.send('thread/start', { cwd: work, ephemeral: false,
    approvalPolicy: 'never', sandbox: 'read-only' })).thread;
  assert.equal(thread.status.type, 'idle');
  assert(result(await r.send('thread/loaded/list')).data.includes(thread.id));
  assert.equal(result(await r.send('thread/read', { threadId: thread.id })).thread.id, thread.id);
  observe('codex', 'spawn_initialize_create_read', { pass: true });
  for (const [phase, cursor] of [['join'], ['inbox'], ['inbox'], ['reply'], ['ack'], ['inbox', 1]]) {
    provider.set(phase, cursor);
    const turn = result(await r.send('turn/start', { threadId: thread.id,
      input: [{ type: 'text', text: 'Disposable fixture ' + phase }] })).turn;
    const done = await until(() => r.events.find(e => e.method === 'turn/completed'
      && e.params.turn.id === turn.id), 'Codex turn');
    assert.equal(done.params.turn.status, 'completed', JSON.stringify(done.params.turn.error));
  }
  checkFixture('codex');
  await idleNotification('codex');
  provider.set('hold');
  const offset = provider.log.length;
  const turn = result(await r.send('turn/start', { threadId: thread.id,
    input: [{ type: 'text', text: 'Disposable busy fixture' }] })).turn;
  await until(() => provider.log.slice(offset).some(e => e.phase === 'hold'), 'Codex provider busy');
  const steer = result(await r.send('turn/steer', { threadId: thread.id, expectedTurnId: turn.id,
    input: [{ type: 'text', text: 'Additional fixture input' }] }));
  assert.equal(steer.turnId, turn.id);
  result(await r.send('turn/interrupt', { threadId: thread.id, turnId: turn.id }));
  const interrupted = await until(() => r.events.find(e => e.method === 'turn/completed'
    && e.params.turn.id === turn.id), 'Codex interrupted event');
  assert.equal(interrupted.params.turn.status, 'interrupted');
  observe('codex', 'busy_steer_interrupt', { accepted_same_turn: true, terminal_status: 'interrupted' });
  await stop(p);
  const resumedProcess = child(exe, ['app-server', '--stdio']), resumedRPC = rpc(resumedProcess);
  result(await resumedRPC.send('initialize', { clientInfo: { name: 'aimem_probe', version: '1' } }));
  resumedRPC.notify('initialized');
  const resumed = result(await resumedRPC.send('thread/resume', { threadId: thread.id }));
  assert.equal(resumed.thread.id, thread.id);
  observe('codex', 'owned_process_restart_resume', { same_id: true });
  await stop(resumedProcess);
}
async function opencode(exe) {
  fs.writeFileSync(path.join(work, 'opencode.json'), JSON.stringify({ enabled_providers: ['openai'],
    model: 'openai/gpt-4.1', small_model: 'openai/gpt-4.1',
    provider: { openai: { options: { baseURL: provider.base, apiKey: 'disposable-not-a-secret' } } },
    mcp: { fixture: { type: 'local', command: [process.execPath, path.join(__dirname, 'mcp.cjs'),
      fixtureFile('opencode')], enabled: true } }, permission: { 'fixture_*': 'allow' } }));
  const listener = net.createServer();
  await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const password = crypto.randomBytes(24).toString('hex');
  const launch = () => {
    const p = child(exe, ['serve', '--pure', '--hostname', '127.0.0.1', '--port', String(port)],
      { OPENCODE_SERVER_PASSWORD: password, OPENCODE_DISABLE_AUTOUPDATE: 'true' });
    p.stdout.resume();
    return p;
  };
  const headers = { Authorization: 'Basic ' + Buffer.from('opencode:' + password).toString('base64'),
    'Content-Type': 'application/json' };
  const base = 'http://127.0.0.1:' + port;
  async function req(url, method = 'GET', body, timeout = 20000) {
    const response = await fetch(base + url, { method, headers,
      body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(timeout) });
    assert(response.ok, method + ' ' + url + ': ' + response.status);
    return response.json();
  }
  async function ready() {
    return until(async () => { try { return await req('/global/health', 'GET', undefined, 1000); }
      catch { return false; } }, 'OpenCode ready');
  }
  const p = launch();
  const health = await ready();
  observe('opencode', 'spawn_health', health);
  const doc = await req('/doc');
  observe('opencode', 'published_spec', { openapi: doc.openapi });
  const controller = new AbortController();
  const stream = await fetch(base + '/event', { headers, signal: controller.signal });
  let events = '';
  const pump = (async () => {
    try { for await (const value of stream.body) events += Buffer.from(value).toString(); }
    catch (error) { if (!controller.signal.aborted) throw error; }
  })();
  try {
    const session = await req('/session', 'POST', { title: 'Disposable protocol fixture' });
    assert.equal((await req('/session/' + session.id)).id, session.id);
    const message = text => req('/session/' + session.id + '/message', 'POST', {
      model: { providerID: 'openai', modelID: 'gpt-4.1' }, parts: [{ type: 'text', text }] });
    for (const [phase, cursor] of [['join'], ['inbox'], ['inbox'], ['reply'], ['ack'], ['inbox', 1]]) {
      provider.set(phase, cursor);
      const response = await message('Disposable fixture ' + phase);
      assert(!response.info?.error, JSON.stringify(response.info?.error));
      assert.equal(response.info?.finish, 'stop');
    }
    checkFixture('opencode');
    await idleNotification('opencode');
    provider.set('hold');
    const offset = provider.log.length;
    const pending = message('Disposable busy fixture');
    // Attach a rejection handler now; await still propagates it after abort.
    pending.catch(() => {});
    await until(() => provider.log.slice(offset).some(e => e.phase === 'hold'), 'OpenCode provider busy');
    assert.equal((await req('/session/status'))[session.id]?.type, 'busy');
    assert.equal(await req('/session/' + session.id + '/abort', 'POST'), true);
    await pending;
    observe('opencode', 'busy_abort', { observed_busy: true, abort_accepted: true });
    await until(() => events.includes('session.created') && events.includes(session.id), 'OpenCode SSE');
    observe('opencode', 'SSE_session_created', { pass: true });
    controller.abort();
    await pump;
    await stop(p);
    const restarted = launch();
    await ready();
    assert.equal((await req('/session/' + session.id)).id, session.id);
    observe('opencode', 'owned_process_restart_read', { same_id: true });
    await req('/session/' + session.id, 'DELETE');
    await stop(restarted);
  } finally { controller.abort(); await pump; }
}
async function main() {
  assert.equal(process.platform, 'win32', 'This discovery fixture has only been qualified on Windows');
  const [codexExe, opencodeExe] = process.argv.slice(2);
  report.codex = await version(codexExe);
  report.opencode = await version(opencodeExe);
  assert.equal(report.codex, 'codex-cli 0.154.0', 'Unqualified Codex version');
  assert.equal(report.opencode, '1.18.3', 'Unqualified OpenCode version');
  provider = await require('./provider.cjs')();
  await codex(codexExe);
  await opencode(opencodeExe);
  report.pass = true;
}
main().catch(error => { report.error = error.message; process.exitCode = 1; }).finally(async () => {
  if (provider) provider.close();
  for (const p of children) {
    try { await stop(p); } catch (error) {
      report.cleanup_error = error.message; report.pass = false; process.exitCode = 1;
    }
  }
  fs.writeFileSync(path.join(root, 'result.json'), JSON.stringify(report, null, 2));
  console.log(JSON.stringify({ evidence_directory: root, ...report }, null, 2));
});
