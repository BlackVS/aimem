// Run with: node scripts/test-epic-details.mjs
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';

const page = readFileSync(new URL('../internal/server/tasks.html', import.meta.url), 'utf8');
const source = page.match(/<script>([\s\S]*?)<\/script>/)[1];
const elements = new Map();
function element(id) {
  if (!elements.has(id)) elements.set(id, {hidden: true, open: false, innerHTML: '', textContent: '', checked: false, disabled: false,
    children: [], style: {}, classList: {remove() {}, toggle() {}}, insertAdjacentHTML() {}});
  return elements.get(id);
}
// The page's listeners are kept, so a test can fire the tab's focus and
// visibility events at them.
const listeners = {document: {}, window: {}};
const listen = on => (type, fn) => { (listeners[on][type] ||= []).push(fn); };
const doc = {getElementById: element, documentElement: {dataset: {}}, visibilityState: 'visible', addEventListener: listen('document')};
const context = vm.createContext({
  document: doc,
  localStorage: {getItem() {return null;}, setItem() {}},
  window: {addEventListener: listen('window')},
  setTimeout() {},
  URLSearchParams,
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

// refreshAll: the epics reload with the view. A fake hub counts the calls
// and serves the epics the test sets.
let calls = [], hubEpics = [], epicsFail = false;
const immediateApi = async path => {
  calls.push(path);
  if (path.includes('/epics')) { if (epicsFail) throw new Error('hub down'); return {epics: hubEpics}; }
  if (path.includes('/tasks/counts')) return {counts: {}, total: 0, archived: 0};
  if (path.includes('/tasks?')) return {tasks: [], next_cursor: ''};
  return {};
};
context.fakeApi = immediateApi;
const settle = () => new Promise(r => setImmediate(r));
const tasksCalls = () => calls.filter(p => p.includes('/tasks?'));
const epicCalls = () => calls.filter(p => p.includes('/epics'));
run(`api = fakeApi; ME = {name: 'op', role: 'admin'}; PROJ = 'p'; VIEW = 'list'; EPICS_PROJ = 'p';
  EPICS = {a: {id: 'a', title: 'Alpha', state: 'OPEN'}, b: {id: 'b', title: 'Beta', state: 'OPEN'}};
  FILTER.epic = 'a'; renderEpicDetails();`);
element('v-list').hidden = false;
element('epicDetails').open = true;
hubEpics = [{id: 'a', title: 'Alpha renamed', state: 'OPEN'}, {id: 'b', title: 'Beta', state: 'OPEN'}, {id: 'c', title: 'Gamma', state: 'OPEN'}];
calls = [];
await run('refreshAll()');
assert.equal(epicCalls().length, 1, 'the refresh reloads the epics');
assert.equal(run('FILTER.epic'), 'a', 'an epic that still exists stays selected');
assert.match(element('epicSel').innerHTML, /value="a" selected>Alpha renamed/);
assert.match(element('epicSel').innerHTML, /Gamma/, 'a new epic appears in the select');
assert.equal(element('epicDetails').open, true, 'the open panel stays open on its epic');
assert.match(element('epicContent').innerHTML, /Alpha renamed/);
assert.equal(tasksCalls().length, 1);
assert.match(tasksCalls()[0], /epic=a/);
assert.ok(calls.indexOf(tasksCalls()[0]) > calls.indexOf(epicCalls()[0]), 'the view loads after the epics');
assert.match(element('loadedAt').textContent, /^loaded /);

// The selected epic is gone: "all epics", a toast, and an unfiltered view.
hubEpics = [{id: 'b', title: 'Beta', state: 'OPEN'}];
calls = []; element('toast').textContent = '';
await run('refreshAll()');
assert.equal(run('FILTER.epic'), '');
assert.match(element('toast').textContent, /no longer exists/);
assert.equal(element('epicDetails').hidden, true);
assert.doesNotMatch(tasksCalls()[0], /epic=/);

// A failed epic reload keeps the epics the page has, and the view still loads.
epicsFail = true; calls = [];
await run(`FILTER.epic = 'b'; refreshAll()`);
assert.equal(run('FILTER.epic'), 'b');
assert.equal(run('EPICS_PROJ'), 'p');
assert.equal(run('Object.keys(EPICS).join()'), 'b');
assert.equal(tasksCalls().length, 1);
epicsFail = false;

// The tab coming back: a fake clock and visibility.
const fire = () => {
  for (const fn of listeners.document.visibilitychange || []) fn();
  for (const fn of listeners.window.focus || []) fn();
};
let now = 1_000_000;
context.fakeNow = () => now;
run('Date.now = fakeNow; LOAD_AT = fakeNow();');
calls = [];
now += 60_000; doc.visibilityState = 'hidden'; fire(); await settle();
assert.equal(calls.length, 0, 'a hidden tab never reloads');
doc.visibilityState = 'visible';
run('LOAD_AT = fakeNow() - 29_000;'); fire(); await settle();
assert.equal(calls.length, 0, 'within 30 s of the last load nothing reloads');
now += 1_000; fire(); await settle();
assert.equal(epicCalls().length, 1, 'visible after 30 s: the epics reload once');
assert.equal(tasksCalls().length, 1, 'visibilitychange and focus together reload the view once');
calls = []; now += 10_000; fire(); await settle();
assert.equal(calls.length, 0, 'the reload just made counts as the last load');
calls = []; now += 60_000; element('v-list').hidden = true; fire(); await settle();
assert.equal(calls.length, 0, 'a task page is never reloaded under an open form');
element('v-list').hidden = false;
// A reload that a filter change made (not refreshAll) also counts as the last load.
run('LOAD_AT = 0;'); calls = [];
await run('listTasks(true)');
calls = []; now += 20_000; fire(); await settle();
assert.equal(calls.length, 0, 'a filter reload 20 s ago is recent enough');
// A press of the button while a reload is in flight is not a second reload.
now += 60_000; calls = [];
await run('Promise.all([refreshAll(), refreshAll()])');
assert.equal(epicCalls().length, 1, 'an in-flight refresh is not duplicated');
assert.equal(tasksCalls().length, 1);
// A view opened while the epics reload has loaded itself: no second view load.
now += 60_000; calls = [];
const pending = run('refreshAll()'); run('NAV++'); await pending;
assert.equal(tasksCalls().length, 0, 'a superseded refresh does not reload the view');
// The board switched to the list while the epics reload, and the reload
// then drops the selected epic: the list the switch loaded used that
// epic, so it loads again without it.
let release;
context.fakeApi = async path => {
  calls.push(path);
  if (path.includes('/epics')) { await new Promise(r => { release = r; }); return {epics: hubEpics}; }
  if (path.includes('/tasks/counts')) return {counts: {}, total: 0, archived: 0};
  if (path.includes('/tasks?')) return {tasks: [], next_cursor: ''};
  return {};
};
hubEpics = [{id: 'a', title: 'Alpha', state: 'OPEN'}, {id: 'b', title: 'Beta', state: 'OPEN'}];
run(`api = fakeApi; EPICS = {a: {id: 'a', title: 'Alpha', state: 'OPEN'}, b: {id: 'b', title: 'Beta', state: 'OPEN'}}; FILTER.epic = 'a'; VIEW = 'board';`);
now += 60_000; calls = [];
const switched = run('refreshAll()');
await settle();
run(`VIEW = 'list'; showList();`); await settle();
assert.ok(tasksCalls().some(p => /epic=a/.test(p)), 'the switch loads the list under the selected epic');
hubEpics = [{id: 'b', title: 'Beta', state: 'OPEN'}];
const before = tasksCalls().length;
release(); await switched; await settle();
assert.equal(run('FILTER.epic'), '');
assert.equal(tasksCalls().length, before + 1, 'the list loads again once the epic is dropped');
assert.doesNotMatch(tasksCalls().at(-1), /epic=/, 'and without the dropped epic');
// The same switch with the epic still present: the list it loaded stands.
run(`FILTER.epic = 'b'; VIEW = 'board';`);
now += 60_000; calls = [];
const kept = run('refreshAll()');
await settle();
run(`VIEW = 'list'; showList();`); await settle();
const listed = tasksCalls().length;
release(); await kept; await settle();
assert.equal(tasksCalls().length, listed, 'a view opened meanwhile is not loaded twice');
// A task opened while the epics reload is never reloaded, even when the
// reload drops the epic.
run(`FILTER.epic = 'b'; VIEW = 'list';`);
now += 60_000; calls = [];
const opened = run('refreshAll()');
await settle();
run('NAV++'); element('v-list').hidden = true;
hubEpics = [];
release(); await opened; await settle();
assert.equal(run('FILTER.epic'), '');
assert.equal(tasksCalls().length, 0, 'a task page is not reloaded under its form');
element('v-list').hidden = false;
context.fakeApi = immediateApi; run('api = fakeApi;');
now += 60_000; calls = [];
run(`VIEW = 'board'`); fire(); await settle();
assert.equal(epicCalls().length, 1, 'the board reloads its epics too');
assert.ok(calls.some(p => p.includes('/tasks?') && p.includes('order=updated_desc')), 'and its cards');
console.log('PASS epic details: open/retired/empty, safe Markdown, selection and project reset');
console.log('PASS refresh: epics reload with the view, a vanished epic falls back, the tab coming back reloads once after 30 s');
