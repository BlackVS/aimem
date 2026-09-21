// Run with: node scripts/test-epic-details.mjs
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';

const page = readFileSync(new URL('../internal/server/tasks.html', import.meta.url), 'utf8');
const source = page.match(/<script>([\s\S]*?)<\/script>/)[1];
const elements = new Map();
function element(id) {
  if (!elements.has(id)) elements.set(id, {hidden: true, open: false, innerHTML: '', classList: {remove() {}}});
  return elements.get(id);
}
const context = vm.createContext({
  document: {getElementById: element, documentElement: {dataset: {}}},
  localStorage: {getItem() {return null;}},
  window: {addEventListener() {}},
});
vm.runInContext(source, context); // Parse and execute the actual embedded page script.
const run = code => vm.runInContext(code, context);
run(`EPICS = {
  open: {title:'Release <one>', state:'OPEN', target:'<img src=x>', objective:'**Goal**\\n<script>alert(1)</script>'},
  retired: {title:'Old milestone', state:'RETIRED', target:'', objective:''}
}; FILTER.epic = 'open'; renderEpicDetails();`);
assert.equal(element('epicDetails').hidden, false);
assert.equal(element('epicDetails').open, false);
assert.match(element('epicContent').innerHTML, /Release &lt;one&gt;/);
assert.match(element('epicContent').innerHTML, /OPEN/);
assert.match(element('epicContent').innerHTML, /Target: &lt;img src=x&gt;/);
assert.match(element('epicContent').innerHTML, /<b>Goal<\/b>/);
assert.doesNotMatch(element('epicContent').innerHTML, /<script>|<img/);

element('epicDetails').open = true;
run(`FILTER.epic = 'retired'; renderEpicDetails();`);
assert.equal(element('epicDetails').open, false);
assert.match(element('epicContent').innerHTML, /RETIRED/);
assert.match(element('epicContent').innerHTML, /No objective provided/);
assert.doesNotMatch(element('epicContent').innerHTML, /Release|Target:/);
run(`FILTER.epic = ''; renderEpicDetails();`);
assert.equal(element('epicDetails').hidden, true);
assert.equal(element('epicContent').innerHTML, '');

run(`FILTER.epic = 'open'; renderEpicDetails(); api = async () => ({epics: []});`);
await run(`loadEpics('another-project')`);
assert.equal(element('epicDetails').hidden, true);
assert.equal(element('epicContent').innerHTML, '');
assert.equal(run('FILTER.epic'), '');
console.log('PASS epic details: open/retired/empty, safe Markdown, selection and project reset');
