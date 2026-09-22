// Scripted Responses SSE, not a model. No request leaves the loopback listener.
const http = require('node:http');
module.exports = async function provider() {
  let phase = 'join', used = false, cursor = 0;
  const log = [];
  const server = http.createServer(async (req, res) => {
    let raw = '';
    for await (const chunk of req) raw += chunk;
    let q;
    try { q = JSON.parse(raw); } catch { res.writeHead(400); res.end(); return; }
    if (req.url.startsWith('/v1/messages/count_tokens')) {
      res.setHeader('Content-Type', 'application/json'); res.end('{"input_tokens":1}'); return;
    }
    const anthropic = req.url.startsWith('/v1/messages');
    if (req.url !== '/v1/responses' && !anthropic) { res.writeHead(404); res.end(); return; }
    const current = phase;
    const definitions = (q.tools || []).flatMap(t => t.type === 'namespace'
      ? t.tools.map(f => ({ ...f, namespace: t.name })) : [t]);
    const tool = definitions.find(t => t.name?.endsWith('probe_' + current));
    const invoke = Boolean(tool && !used);
    if (invoke) used = true;
    log.push({ phase: current, invoke, name: tool?.name });
    const args = JSON.stringify(current === 'join' ? { label: 'fixture' }
      : current === 'reply' ? { message_id: 'fixture-message', answer: 'fixture answer' }
      : current === 'ack' ? { message_id: 'fixture-message' } : { cursor });
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const emit = (type, value) => res.write('event: ' + type + '\ndata: '
      + JSON.stringify({ type, ...value }) + '\n\n');
    // Give the controller a bounded interval to observe and interrupt a busy turn.
    if (current === 'hold') await new Promise(resolve => setTimeout(resolve, 5000));
    if (res.destroyed) return;
    const id = 'resp_' + log.length, call = 'call_' + log.length;
    const base = { id, object: 'response', created_at: Math.floor(Date.now() / 1000),
      model: 'fixture', status: 'in_progress', output: [] };
    if (anthropic) {
      emit('message_start', { message: { id: 'msg_' + id, type: 'message', role: 'assistant',
        model: q.model, content: [], stop_reason: null, stop_sequence: null,
        usage: { input_tokens: 1, output_tokens: 0 } } });
      emit('content_block_start', { index: 0, content_block: invoke
        ? { type: 'tool_use', id: call, name: tool.name, input: {} } : { type: 'text', text: '' } });
      emit('content_block_delta', { index: 0, delta: invoke
        ? { type: 'input_json_delta', partial_json: args } : { type: 'text_delta', text: 'FIXTURE_DONE' } });
      emit('content_block_stop', { index: 0 });
      emit('message_delta', { delta: { stop_reason: invoke ? 'tool_use' : 'end_turn', stop_sequence: null },
        usage: { output_tokens: 1 } });
      emit('message_stop', {});
      res.end();
      return;
    }
    emit('response.created', { response: base });
    let item;
    if (invoke) {
      item = { id: 'fc_' + call, type: 'function_call', call_id: call,
        name: tool.name, namespace: tool.namespace, arguments: args, status: 'completed' };
      emit('response.output_item.added', { output_index: 0,
        item: { ...item, arguments: '', status: 'in_progress' } });
      emit('response.function_call_arguments.delta', { output_index: 0, item_id: item.id, delta: args });
      emit('response.function_call_arguments.done', { output_index: 0, item_id: item.id, arguments: args });
    } else {
      item = { id: 'msg_' + call, type: 'message', role: 'assistant', status: 'completed',
        content: [{ type: 'output_text', text: 'FIXTURE_DONE', annotations: [] }] };
      emit('response.output_item.added', { output_index: 0,
        item: { ...item, status: 'in_progress', content: [] } });
      emit('response.content_part.added', { output_index: 0, item_id: item.id, content_index: 0,
        part: { type: 'output_text', text: '', annotations: [] } });
      emit('response.output_text.delta', { output_index: 0, item_id: item.id,
        content_index: 0, delta: 'FIXTURE_DONE' });
      emit('response.output_text.done', { output_index: 0, item_id: item.id,
        content_index: 0, text: 'FIXTURE_DONE' });
    }
    emit('response.output_item.done', { output_index: 0, item });
    emit('response.completed', { response: { ...base, status: 'completed', output: [item],
      usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } });
    res.end();
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {
    base: 'http://127.0.0.1:' + server.address().port + '/v1', log,
    set(next, nextCursor = 0) { phase = next; used = false; cursor = nextCursor; },
    close() { server.closeAllConnections(); server.close(); },
  };
};
