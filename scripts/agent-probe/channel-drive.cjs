// Driver for a human-launched disposable Claude session using channel.cjs.
// Usage (RUN is the disposable run directory that holds state.json and debug.log):
//   node channel-drive.cjs emit RUN ID [TEXT]   push one synthetic channel event
//   node channel-drive.cjs exit RUN             stop the channel server (simulated crash)
//   node channel-drive.cjs report RUN           merged timeline of fixture state and debug log
// It reads only the run directory; it never contacts a hub or a model.
const fs = require('node:fs');
const path = require('node:path');
const [command, run, id, ...text] = process.argv.slice(2);
if (!run) throw Error('usage: channel-drive.cjs emit|exit|report RUN [ID [TEXT]]');
const stateFile = path.join(run, 'state.json');
let n = 0;
const drop = event => {
  const name = stateFile + '.event.' + Date.now() + '-' + String(n++).padStart(3, '0');
  fs.writeFileSync(name + '.tmp', JSON.stringify(event));
  fs.renameSync(name + '.tmp', name); // The server ignores the .tmp file until it is complete.
};

if (command === 'emit') {
  if (!id) throw Error('emit needs a message id');
  drop({ id, content: text.join(' ') || 'synthetic fixture question ' + id });
} else if (command === 'exit') {
  drop({ exit: true });
} else if (command === 'report') {
  const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  const timeline = [];
  for (const e of state.emitted) timeline.push({ at: e.at, source: 'fixture', what: 'emitted', id: e.id, epoch: e.epoch });
  for (const c of state.calls) {
    timeline.push({ at: c.at, source: 'fixture', what: 'call ' + c.name, args: c.args, epoch: c.epoch });
    if (c.done) timeline.push({ at: c.done, source: 'fixture', what: 'done ' + c.name, epoch: c.epoch });
  }
  if (state.exited) timeline.push({ at: state.exited.at, source: 'fixture', what: 'server exited', epoch: state.exited.epoch });
  const debug = path.join(run, 'debug.log');
  const keep = /Channel |channel notifications|notifications\/claude\/channel|\[API REQUEST\]|\[engine\] turn \d+ (start|end)|reconnect|transport closed|Connection established|Successfully connected/;
  if (fs.existsSync(debug)) {
    for (const line of fs.readFileSync(debug, 'utf8').split(/\r?\n/)) {
      const m = line.match(/^(\S+Z) \[(\w+)\] (.*)$/);
      if (m && keep.test(m[3])) timeline.push({ at: Date.parse(m[1]), source: 'claude', what: m[3].slice(0, 200) });
    }
  }
  timeline.sort((a, b) => a.at - b.at);
  const t0 = timeline.length ? timeline[0].at : 0;
  for (const t of timeline) t.t = ((t.at - t0) / 1000).toFixed(3);
  console.log(JSON.stringify({ starts: state.starts, initialize: state.initialize, timeline }, null, 1));
} else {
  throw Error('unknown command ' + command);
}
