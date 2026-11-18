// Run with: node scripts/test-features-view.mjs
// The board page's features view against a stand-in for the hub's API.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';

const page = readFileSync(new URL('../internal/server/tasks.html', import.meta.url), 'utf8');
const source = page.match(/<script>([\s\S]*?)<\/script>/)[1];
const elements = new Map();
function element(id) {
  if (!elements.has(id)) elements.set(id, {hidden: true, open: false, innerHTML: '', textContent: '', value: '', checked: false, disabled: false,
    children: [], style: {}, classList: {add() {}, remove() {}, toggle() {}}, insertAdjacentHTML() {}});
  return elements.get(id);
}
const location = {search: ''};
const pushed = [];
const context = vm.createContext({
  document: {getElementById: element, documentElement: {dataset: {}}, visibilityState: 'visible', addEventListener() {}},
  localStorage: {getItem() {return null;}, setItem() {}, removeItem() {}},
  window: {addEventListener() {}},
  history: {pushState(_s, _t, url) { pushed.push(url); location.search = url.slice(url.indexOf('?')); }},
  location,
  setTimeout() {},
  URLSearchParams,
});
vm.runInContext(source, context);
const run = code => vm.runInContext(code, context);

// The stand-in hub. Project p has goal g with three features: two share
// position 2. Task t-other lives in project q; t-hidden is one the reader
// may not read, which the hub answers as "not found".
const notFound = () => Object.assign(new Error('not found'), {status: 404});
const hub = {
  epics: [{id: 'g', title: 'Goal <one>, ' + 'with a title long enough to wrap over more than one line of the page; '.repeat(3), state: 'OPEN',
    objective: ['First paragraph of the goal.', 'Second paragraph with <script>x</script> in it.', 'The check: **every** feature is DONE.'].join('\n\n')},
    {id: 'empty', title: 'Empty goal', state: 'OPEN'}],
  features: {
    g: [
      {id: 'f1', title: 'First <b>feature</b>', state: 'IN_PROGRESS', archived: false, feature: {goal: 'g', position: 1, mark: 'mandatory'}, confirmed_tasks: 4},
      {id: 'f2', title: 'Second', state: 'READY', archived: false, feature: {goal: 'g', position: 2, mark: 'optional'}, confirmed_tasks: 1, tie: true},
      {id: 'f3', title: 'Third', state: 'BACKLOG', archived: false, feature: {goal: 'g', position: 2, mark: 'optional'}, confirmed_tasks: 0, tie: true},
    ],
    empty: [],
  },
  records: {
    f1: {id: 'f1', dependencies: ['t-done', 't-open', 't-other', 't-hidden']},
    f2: {id: 'f2', dependencies: ['t-done']},
    't-other': {id: 't-other', title: 'Work in <q>', state: 'REVIEW', project: 'q'},
  },
  local: {
    f1: [{id: 't-done', title: 'Done work', state: 'DONE'}, {id: 't-open', title: 'Open work', state: 'BLOCKED'}],
    f2: [{id: 't-done', title: 'Done work', state: 'DONE'}],
  },
  featuresRoute: true,
};
let calls = [];
context.fakeApi = async path => {
  calls.push(path);
  const u = new URL(path, 'http://hub');
  let m;
  if (u.pathname === '/v1/access/identity') return {name: 'reader', role: 'user', tasks_enabled: true, task_write: false};
  if ((m = u.pathname.match(/^\/v1\/projects\/p\/epics\/([^/]+)\/features$/))) {
    if (!hub.featuresRoute) throw notFound();
    return {project: 'p', goal: m[1], features: hub.features[m[1]] || []};
  }
  if (u.pathname === '/v1/projects/p/epics') return {epics: hub.epics};
  if (u.pathname === '/v1/projects/p/tasks') return {tasks: hub.local[u.searchParams.get('feature')] || [], next_cursor: ''};
  if ((m = u.pathname.match(/^\/v1\/tasks\/([^/]+)$/))) { if (hub.records[m[1]]) return hub.records[m[1]]; throw notFound(); }
  throw new Error('unexpected call ' + path);
};
run(`api = fakeApi; ME = {name: 'reader', role: 'user'}; PROJECTS = null;`);
const settle = () => new Promise(r => setImmediate(r));
const open = async search => { location.search = search; calls = []; run('route()'); for (let i = 0; i < 5; i++) await settle(); };
const body = () => element('featBody').innerHTML;

// The address opens the view: project, view, goal.
await open('?project=p&view=features&goal=g');
assert.equal(run('VIEW'), 'features');
assert.equal(run('GOAL'), 'g');
assert.equal(element('features').hidden, false, 'the features view is shown');
assert.equal(element('list').hidden, true, 'apart from the tasks');
assert.equal(element('board').hidden, true);
assert.equal(element('epicFilter').hidden, true, 'the epic filter belongs to the task views');
assert.equal(element('goalFilter').hidden, false, 'the goal chooser is shown');
// The chosen goal in the task views' panel: its title in full, its state and
// its description rendered, open with the view.
const panel = element('epicDetails'), goalText = () => element('epicContent').innerHTML;
assert.equal(panel.hidden, false, "the goal's details are shown");
assert.equal(panel.open, true, 'and open');
assert.equal(element('epicSummary').textContent, 'View goal');
assert.match(goalText(), new RegExp('<h2>Goal &lt;one&gt;, ' + '(with a title long enough to wrap over more than one line of the page; ){3}</h2>'), 'the whole title');
assert.match(goalText(), /<div class="meta">OPEN<\/div>/);
assert.equal((goalText().match(/<p>/g) || []).length, 3, 'each paragraph of the description');
assert.match(goalText(), /&lt;script&gt;/, 'the description is escaped');
assert.match(goalText(), /<b>every<\/b>/, 'and rendered as in the task views');
assert.match(goalText(), /The features below belong to this goal/);
assert.match(element('goalSel').innerHTML, /value="g" selected>Goal &lt;one&gt;/);
const order = [...body().matchAll(/class="ftitle" data-go href="([^"]+)">([^<]*(?:<(?!\/a>)[^<]*)*)<\/a>/g)].map(m => m[1]);
assert.deepEqual(order, ['f1', 'f2', 'f3'].map(id => `/tasks?project=p&view=features&goal=g&feature=${id}`), 'features in position order, each linked');
assert.match(body(), /First &lt;b&gt;feature&lt;\/b&gt;/, 'titles are escaped');
assert.equal((body().match(/class="frow/g) || []).length, 3, 'each feature in its own colour');
assert.equal((body().match(/>tie</g) || []).length, 2, 'the two features at position 2 are a tie');
assert.match(body(), /mark-mandatory">mandatory/);
assert.match(body(), /4 confirmed/);
// What f1 waits on: not the DONE task; the open one, the foreign one with
// its project, and the unreadable one.
const f1 = body().split('class="frow')[1];
assert.doesNotMatch(f1, /Done work/, 'a DONE task is not waited on');
assert.match(f1, /Open work<\/a> <span class="chip st st-BLOCKED">/);
assert.match(f1, /Work in &lt;q&gt;<\/a> <span class="chip st st-REVIEW">REVIEW<\/span> <span class="dim">q<\/span>/, 'a task of another project, with its project');
assert.match(f1, /not readable or not found<\/span> <code>t-hidden<\/code>/, 'an unreadable task is shown, not left out');
assert.match(body().split('class="frow')[2], /waits on nothing: every confirmed task is DONE/);
assert.match(body().split('class="frow')[3], /no confirmed tasks/);
assert.ok(!calls.some(p => p.includes('/v1/tasks/f3') || p.includes('feature=f3')), 'a feature without confirmed tasks costs no call');
assert.ok(!calls.some(p => p === '/v1/tasks/t-open'), "this project's tasks come from the list, not one read each");
assert.match(element('loadedAt').textContent, /^loaded /);

// A reader who closed the panel keeps it closed while the goal stays.
panel.open = false;
await open('?project=p&view=features&goal=g&feature=f1');
assert.equal(panel.open, false, 'closed by the reader, closed on the same goal');
await open('?project=p&view=features&goal=g');
// The task views get their own panel back: no epic filter, no panel.
await open('?project=p');
assert.equal(panel.hidden, true, 'the goal does not follow into the list');
assert.equal(element('epicSummary').textContent, 'View epic');
assert.equal(element('goalFilter').hidden, true);
await open('?project=p&view=features&goal=g');
assert.equal(panel.open, true, 'a goal shown anew opens again');

// One feature opened: its confirmed tasks with state and project.
await open('?project=p&view=features&goal=g&feature=f1');
assert.equal(run('FEATURE'), 'f1');
assert.equal(run('listURL()'), '/tasks?project=p&view=features&goal=g&feature=f1', 'the view is its own address');
assert.match(body(), /← the goal's features/);
assert.match(body(), /<h2>confirmed tasks<\/h2>/);
const rows = body().split('class="ftask"').slice(1);
assert.equal(rows.length, 4, 'every confirmed task, DONE included');
assert.match(rows[0], /Done work<\/a> <span class="chip st st-DONE">DONE<\/span> <span class="dim">p<\/span>/);
assert.match(rows[2], /<span class="dim">q<\/span>/);
assert.match(rows[3], /not readable or not found/);
await open('?project=p&view=features&goal=g&feature=nope');
assert.match(body(), /This goal has no such feature/);

// Empty views say so.
await open('?project=p&view=features&goal=empty');
assert.match(body(), /This goal has no features/);
await open('?project=p&view=features');
assert.match(body(), /Choose a goal to see its features/);
assert.equal(panel.hidden, true, 'no goal chosen, no goal panel');
// Not even with an epic chosen in the task views' filter.
run(`EPICS = {g: {id: 'g', title: 'An epic', state: 'OPEN'}}; FILTER.epic = 'g'`);
await open('?project=p&view=features');
assert.equal(panel.hidden, true, "the filter's epic is not shown as the goal");
// And back in the list, the filter's epic is in its panel again, whatever
// the features view showed: no goal, a goal then none, an unknown goal.
const epicBack = async why => {
  await open('?project=p');
  assert.equal(panel.hidden, false, why + ': the epic panel is back');
  assert.equal(element('epicSummary').textContent, 'View epic', why);
  assert.match(goalText(), /<h2>An epic<\/h2>[\s\S]*Tasks below belong to this epic/, why);
};
await epicBack('after no goal');
await open('?project=p&view=features&goal=g');
assert.equal(element('epicSummary').textContent, 'View goal');
run(`setGoal('')`); for (let i = 0; i < 5; i++) await settle();
await epicBack('after a goal, then none');
await open('?project=p&view=features&goal=gone');
await epicBack('after an unknown goal');
run(`FILTER.epic = ''`);
// Inside the view, the goal's panel stays while the address names its goal:
// it is not hidden while the view reloads, only when the goal changes.
await open('?project=p&view=features&goal=g');
location.search = '?project=p&view=features&goal=g&feature=f1'; run('route()');
assert.equal(panel.hidden, false, 'the same goal: the panel stays while the feature loads');
for (let i = 0; i < 5; i++) await settle();
assert.equal(panel.hidden, false);
location.search = '?project=p&view=features&goal=empty'; run('route()');
assert.equal(panel.hidden, true, 'another goal: hidden until it is rendered');
for (let i = 0; i < 5; i++) await settle();
await open('?project=p&view=features&goal=gone');
assert.match(body(), /This project has no such goal/);
hub.epics = [];
await open('?project=p&view=features&goal=g');
assert.match(body(), /This project has no goals/);
hub.epics = [{id: 'g', title: 'Goal', state: 'OPEN'}];

// A hub whose features route answers "not found": the view says so and
// the rest of the page works.
hub.featuresRoute = false;
await open('?project=p&view=features&goal=g');
assert.match(body(), /Features are not available on this hub/);
await open('?project=p');
assert.equal(run('VIEW'), 'list');
assert.equal(element('features').hidden, true);
assert.equal(element('list').hidden, false);
assert.ok(calls.some(p => p.startsWith('/v1/projects/p/tasks?')), 'the list still loads');
hub.featuresRoute = true;

// Choosing a goal and switching views keep the address the state.
await open('?project=p&view=features&goal=g&feature=f1');
pushed.length = 0;
run(`setGoal('empty')`); for (let i = 0; i < 5; i++) await settle();
assert.equal(pushed[0], '/tasks?project=p&view=features&goal=empty', 'a new goal closes the feature');
run(`setView('list')`);
assert.equal(pushed.at(-1), '/tasks?project=p');
run(`setView('features')`);
assert.equal(pushed.at(-1), '/tasks?project=p&view=features&goal=empty', 'the goal is kept across a visit to the list');
run(`setProject('other'); setView('features')`);
assert.equal(pushed.at(-1), '/tasks?project=other&view=features', 'a goal belongs to its project');
run(`setProject('p')`);

// A plain click on a link of the view navigates in the page; a modified
// click is left to the browser.
const link = {getAttribute: () => '/tasks?project=p&view=features&goal=g&feature=f2'};
let prevented = 0;
const click = mods => run('featClick')(Object.assign({target: {closest: () => link}, button: 0, preventDefault() { prevented++; }}, mods));
pushed.length = 0;
click({}); for (let i = 0; i < 5; i++) await settle();
assert.equal(prevented, 1);
assert.equal(pushed[0], '/tasks?project=p&view=features&goal=g&feature=f2');
click({ctrlKey: true});
assert.equal(prevented, 1, 'a ctrl-click opens a tab as usual');

console.log('PASS features view: the goal details, order and tie, waits-on, another project and an unreadable task, empty views, missing route, the address');
