// Disposable MCP server. This is deliberately not the aimem team protocol.
const fs = require('node:fs');
const readline = require('node:readline');
const stateFile = process.argv[2];
let state = { joined: false, answered: false, acked: false, calls: [] };
if (fs.existsSync(stateFile)) state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
const save = () => fs.writeFileSync(stateFile, JSON.stringify(state));
const send = value => process.stdout.write(JSON.stringify(value) + '\n');
const tools = ['join', 'inbox', 'reply', 'ack'].map(name => ({
  name: 'probe_' + name,
  description: 'Disposable coordination fixture ' + name,
  inputSchema: {
    type: 'object',
    properties: { label: { type: 'string' }, cursor: { type: 'integer' },
      message_id: { type: 'string' }, answer: { type: 'string' } },
    additionalProperties: false,
  },
}));

async function dispatch(q) {
  if (q.method === 'initialize') {
    state.protocol = q.params.protocolVersion;
    save();
    return { protocolVersion: state.protocol, capabilities: { tools: {}, logging: {} },
      serverInfo: { name: 'aimem-probe', version: '1' } };
  }
  if (q.method === 'tools/list') return { tools };
  if (q.method === 'ping' || q.method === 'logging/setLevel') return {};
  if (q.method !== 'tools/call') throw Error('Unknown method');
  const { name, arguments: args } = q.params;
  let data;
  switch (name) {
    case 'probe_join':
      state.joined = true;
      data = { session_id: 'fixture-session', generation: 1, waiting: true };
      break;
    case 'probe_inbox':
      if (!state.joined || ![0, 1].includes(args.cursor)) throw Error('Invalid inbox request');
      await new Promise(resolve => setTimeout(resolve, 100));
      data = { messages: state.acked ? [] : [{ id: 'fixture-message', kind: 'question',
        payload: { text: 'fixture question' } }], next_cursor: 1 };
      break;
    case 'probe_reply':
      if (!state.joined || args.message_id !== 'fixture-message' || args.answer !== 'fixture answer') {
        throw Error('Invalid reply correlation');
      }
      state.answered = true;
      data = { in_reply_to: args.message_id, kind: 'answer', accepted: true };
      break;
    case 'probe_ack':
      if (!state.answered || args.message_id !== 'fixture-message') throw Error('Invalid ack');
      state.acked = true;
      data = { acked: [args.message_id] };
      break;
    default: throw Error('Unknown tool');
  }
  state.calls.push({ name, args, result: data });
  save();
  return { content: [{ type: 'text', text: JSON.stringify(data) }] };
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
// A file trigger lets the runner emit a notification while the client is idle.
const timer = setInterval(() => {
  if (!fs.existsSync(stateFile + '.notify')) return;
  fs.unlinkSync(stateFile + '.notify');
  send({ jsonrpc: '2.0', method: 'notifications/message',
    params: { level: 'info', logger: 'fixture', data: 'fixture inbox changed' } });
  state.notifications = (state.notifications || 0) + 1;
  save();
}, 100);
input.on('close', () => clearInterval(timer));
