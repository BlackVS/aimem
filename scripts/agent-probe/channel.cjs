// Disposable Claude channel server. Deliberately not the aimem team protocol.
// argv[2] is a state file; the driver drops JSON event files next to it
// (<state>.event.<name>) and this server pushes each as a channel notification.
const fs = require('node:fs');
const path = require('node:path');
const readline = require('node:readline');
const stateFile = process.argv[2];
const dir = path.dirname(stateFile), prefix = path.basename(stateFile) + '.event.';
let state = { starts: 0, emitted: [], calls: [], initialize: [] };
if (fs.existsSync(stateFile)) state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
state.starts++;
const epoch = state.starts;
const save = () => fs.writeFileSync(stateFile, JSON.stringify(state, null, 1));
save();
const send = value => process.stdout.write(JSON.stringify(value) + '\n');
const schema = props => ({ type: 'object', properties: props, additionalProperties: false });
const tools = [
  { name: 'probe_ack', description: 'Record receipt of fixture channel messages by id',
    inputSchema: schema({ message_ids: { type: 'array', items: { type: 'string' } } }) },
  { name: 'probe_reply', description: 'Answer one fixture channel message',
    inputSchema: schema({ message_id: { type: 'string' }, answer: { type: 'string' } }) },
  { name: 'probe_wait', description: 'Wait the given seconds (1-30), then return; keeps a turn busy',
    inputSchema: schema({ seconds: { type: 'integer' } }) },
];

async function dispatch(q) {
  if (q.method === 'initialize') {
    state.initialize.push({ epoch, protocol: q.params.protocolVersion,
      client: q.params.clientInfo, capabilities: q.params.capabilities });
    save();
    return { protocolVersion: q.params.protocolVersion,
      capabilities: { tools: {}, experimental: { 'claude/channel': {} } },
      serverInfo: { name: 'aimem-channel-probe', version: '1' },
      instructions: 'Synthetic fixture events arrive as <channel source="fixture" message_id="...">. '
        + 'For each new message_id: call probe_ack once with the ids, then probe_reply once per id '
        + 'with a one-line answer. Do nothing else for these events.' };
  }
  if (q.method === 'tools/list') return { tools };
  if (q.method === 'ping') return {};
  if (q.method !== 'tools/call') throw Error('Unknown method ' + q.method);
  const { name, arguments: args } = q.params;
  if (!tools.some(t => t.name === name)) throw Error('Unknown tool');
  const call = { epoch, name, args, at: Date.now() };
  state.calls.push(call);
  save();
  if (name === 'probe_wait') {
    const seconds = Math.min(30, Math.max(1, Number(args?.seconds) || 1));
    await new Promise(resolve => setTimeout(resolve, seconds * 1000));
    call.done = Date.now();
    save();
  }
  return { content: [{ type: 'text', text: JSON.stringify({ recorded: name }) }] };
}

const input = readline.createInterface({ input: process.stdin });
input.on('line', async line => {
  let q;
  try {
    q = JSON.parse(line);
    if (q.id === undefined) return;
    send({ jsonrpc: '2.0', id: q.id, result: await dispatch(q) });
  } catch (error) {
    send({ jsonrpc: '2.0', id: q?.id ?? null, error: { code: -32603, message: error.message } });
  }
});
// Event files are consumed in name order. An event {"exit": true} stops this
// process without closing Claude Code, simulating a crashed channel server.
const timer = setInterval(() => {
  for (const f of fs.readdirSync(dir).filter(n => n.startsWith(prefix) && !n.endsWith('.tmp')).sort()) {
    const file = path.join(dir, f), event = JSON.parse(fs.readFileSync(file, 'utf8'));
    fs.unlinkSync(file);
    if (event.exit) { state.exited = { epoch, at: Date.now() }; save(); process.exit(0); }
    send({ jsonrpc: '2.0', method: 'notifications/claude/channel',
      params: { content: event.content, meta: { message_id: event.id, sender: 'fixture' } } });
    state.emitted.push({ epoch, id: event.id, at: Date.now() });
    save();
  }
}, 50);
input.on('close', () => clearInterval(timer));
