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
  epics: [{id: 'old', title: 'Older epic', state: 'OPEN'}, {id: 'g', title: 'Goal <one>, ' + 'with a title long enough to wrap over more than one line of the page; '.repeat(3), state: 'OPEN',
    objective: ['First paragraph of the goal.', 'Second paragraph with <script>x</script> in it.', 'The check: **every** feature is DONE.'].join('\n\n')},
    {id: 'empty', title: 'Empty goal', state: 'OPEN'}, {id: 'r', title: 'Retired goal', state: 'RETIRED'}],
  features: {
    g: [
      {id: 'f1', title: 'First <b>feature</b>', state: 'IN_PROGRESS', archived: false, feature: {goal: 'g', position: 1, mark: 'mandatory'}, confirmed_tasks: 4},
      {id: 'f2', title: 'Second', state: 'READY', archived: false, feature: {goal: 'g', position: 2, mark: 'optional'}, confirmed_tasks: 1, tie: true},
      {id: 'f3', title: 'Third', state: 'BACKLOG', archived: false, feature: {goal: 'g', position: 2, mark: 'optional'}, confirmed_tasks: 0, tie: true},
    ],
    empty: [],
    r: [{id: 'fr', title: 'Archived', state: 'CANCELLED', archived: true, feature: {goal: 'r', position: 1, mark: 'optional'}, confirmed_tasks: 0}],
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
  kindFeature: true,
};
// The project's feature records as the task list serves them with
// kind=feature: summary rows with kind and epic and no feature link (as
// the hub serves them), two to a page, so the page follows the cursor; the
// last page has no next_cursor.
const featurePage = after => {
  const all = Object.values(hub.features).flat().map(f => ({id: f.id, title: f.title, state: f.state, archived: f.archived, kind: 'feature', epic: f.feature.goal}));
  const i = after ? Number(after) : 0;
  return i + 2 < all.length ? {tasks: all.slice(i, i + 2), next_cursor: String(i + 2)} : {tasks: all.slice(i)};
};
let calls = [];
context.fakeApi = async path => {
  calls.push(path);
  const u = new URL(path, 'http://hub');
  let m;
  if (u.pathname === '/v1/access/identity') return {name: 'reader', role: 'user', tasks_enabled: true, task_write: false};
  if (u.pathname === '/v1/projects/q/epics') return {epics: [{id: 'gq', title: 'Goal of q', state: 'OPEN'}]};
  if (/^\/v1\/tasks\/[^/]+\/(history|comments)$/.test(u.pathname)) return {revisions: [], comments: [], next_cursor: ''};
  if ((m = u.pathname.match(/^\/v1\/projects\/p\/epics\/([^/]+)\/features$/))) {
    if (!hub.featuresRoute) throw notFound();
    return {project: 'p', goal: m[1], features: hub.features[m[1]] || []};
  }
  if (u.pathname === '/v1/projects/p/epics') return {epics: hub.epics};
  if (u.pathname === '/v1/projects/p/tasks' && u.searchParams.get('kind') === 'feature') {
    if (!hub.kindFeature) throw Object.assign(new Error('bad kind'), {status: 400});
    if (hub.ignoresKind) return {tasks: [{id: 't-plain', title: 'A task', state: 'READY', epic: 'old'}]};
    assert.equal(u.searchParams.get('include_archived'), 'true');
    return featurePage(u.searchParams.get('after'));
  }
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
// its description rendered, closed so that the features are on the first
// screen, also when the address names the goal.
const panel = element('epicDetails'), goalText = () => element('epicContent').innerHTML;
assert.equal(panel.hidden, false, "the goal's details are shown");
assert.equal(panel.open, false, 'and closed');
assert.equal(element('epicSummary').textContent, 'View goal');
assert.match(goalText(), new RegExp('<h2>Goal &lt;one&gt;, ' + '(with a title long enough to wrap over more than one line of the page; ){3}</h2>'), 'the whole title');
assert.match(goalText(), /<div class="meta">OPEN<\/div>/);
assert.equal((goalText().match(/<p>/g) || []).length, 3, 'each paragraph of the description');
assert.match(goalText(), /&lt;script&gt;/, 'the description is escaped');
assert.match(goalText(), /<b>every<\/b>/, 'and rendered as in the task views');
assert.match(goalText(), /The features below belong to this goal/);
// The selector: the goals with features first, each with its number in
// front of its title (archived features included, as the view lists
// them), the retired mark in the same bracket; the epics without features
// after them, with no label or separator between. A long title is cut,
// and the option's tooltip keeps it whole.
const escText = t => t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const gTitle = hub.epics.find(e => e.id === 'g').title;
const gShown = '(3) ' + escText(Array.from(gTitle).slice(0, 59).join('') + '…');
const options = [...element('goalSel').innerHTML.matchAll(/<option value="([^"]*)"[^>]*>([^<]*)<\/option>/g)].map(m => [m[1], m[2]]);
assert.deepEqual(options, [['', '— choose a goal —'], ['g', gShown], ['r', '(1, retired) Retired goal'], ['old', 'Older epic'], ['empty', 'Empty goal']]);
assert.ok(element('goalSel').innerHTML.includes(`<option value="g" title="${escText(gTitle).replace(/"/g, '&quot;')}" selected>`), 'the whole title is the tooltip');
assert.doesNotMatch(element('goalSel').innerHTML, /optgroup|no features yet/, 'no group label');
assert.ok(goalText().includes(escText(gTitle).trim()), 'the goal panel shows the whole title');
// A title of 120 characters: cut to GOAL_TITLE_MAX with an ellipsis, the
// count still in front; a retired and a reached epic, each with and without
// features, marked apart.
const long = 'A goal whose title is a whole sentence about what the goal is for, written where its objective should have said it whole';
assert.equal(long.length, 120);
const opts = html => [...html.matchAll(/<option value="([^"]*)" title="([^"]*)"[^>]*>([^<]*)<\/option>/g)].map(m => [m[1], m[2], m[3]]);
assert.deepEqual(opts(run(`goalOptions(${JSON.stringify([{id: 'x', title: long, state: 'OPEN'}, {id: 'y', title: 'Old', state: 'RETIRED'}, {id: 'z', title: 'Gone', state: 'RETIRED'}, {id: 'd', title: 'Done', state: 'REACHED'}, {id: 'e', title: 'Met', state: 'REACHED'}])}, {x: {count: 5}, z: {count: 1}, d: {count: 2}})`)), [
  ['x', long, '(5) ' + long.slice(0, 59) + '…'],
  ['z', 'Gone', '(1, retired) Gone'],
  ['d', 'Done', '(2, reached) Done'],
  ['y', 'Old', '(retired) Old'],
  ['e', 'Met', '(reached) Met'],
]);
assert.equal(run('GOAL_TITLE_MAX'), 60);
assert.equal(Array.from(run(`goalTitle(${JSON.stringify(long)})`)).length, 60, 'the cut title, ellipsis included, is GOAL_TITLE_MAX long');
assert.equal(run('goalTitle("A title of exactly sixty characters, which is not cut at all")'), 'A title of exactly sixty characters, which is not cut at all');
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
assert.match(body().split('class="frow')[2], /no confirmed task is open: its one confirmed task is DONE, and the feature is still READY/, 'an open feature is not shown as finished');
assert.doesNotMatch(body(), /waits on nothing/);
assert.match(body().split('class="frow')[3], /no confirmed tasks/);
assert.ok(!calls.some(p => p.includes('/v1/tasks/f3') || p.includes('feature=f3')), 'a feature without confirmed tasks costs no call');
assert.ok(!calls.some(p => p === '/v1/tasks/t-open'), "this project's tasks come from the list, not one read each");
assert.match(element('loadedAt').textContent, /^loaded /);

// A reader who opened the panel keeps it open while the goal stays: a visit
// to a feature and back, and a reload of the features.
panel.open = true;
await open('?project=p&view=features&goal=g&feature=f1');
assert.equal(panel.open, true, 'opened by the reader, open on the same goal');
await open('?project=p&view=features&goal=g');
assert.equal(panel.open, true, 'and on the way back');
run('refreshView()'); for (let i = 0; i < 5; i++) await settle();
assert.equal(panel.open, true, 'and across a reload');
// Another goal starts closed, and so does the first one chosen again.
run(`setGoal('empty')`); for (let i = 0; i < 5; i++) await settle();
assert.equal(panel.open, false, 'another goal is closed');
panel.open = true;
run(`setGoal('g')`); for (let i = 0; i < 5; i++) await settle();
assert.equal(panel.open, false, 'a goal chosen again is closed');
// A DONE feature whose confirmed tasks are DONE keeps the line it had.
hub.features.g[1].state = 'DONE';
await open('?project=p&view=features&goal=g');
assert.match(body().split('class="frow')[2], /waits on nothing: every confirmed task is DONE/);
hub.features.g[1].state = 'READY';
// Two confirmed tasks, both DONE, on an open feature.
hub.records.f2.dependencies = ['t-done', 't-done2'];
hub.local.f2 = [{id: 't-done', title: 'Done work', state: 'DONE'}, {id: 't-done2', title: 'More done work', state: 'DONE'}];
await open('?project=p&view=features&goal=g&feature=f2');
assert.match(body(), /no confirmed task is open: all 2 are DONE, and the feature is still READY/);
hub.records.f2.dependencies = ['t-done']; hub.local.f2 = [{id: 't-done', title: 'Done work', state: 'DONE'}];
await open('?project=p&view=features&goal=g');
// The task views get their own panel back: no epic filter, no panel.
await open('?project=p');
assert.equal(panel.hidden, true, 'the goal does not follow into the list');
assert.equal(element('epicSummary').textContent, 'View epic');
assert.equal(element('goalFilter').hidden, true);
await open('?project=p&view=features&goal=g');
assert.equal(panel.open, false, 'a goal shown anew starts closed');

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
assert.equal(body(), '<div class="dim">This epic has no feature records. The goals that have them come first in the goal selector, each with its number of features in brackets.</div>');
await open('?project=p&view=features');
assert.match(body(), /Choose a goal to see its features/);
assert.deepEqual(calls.filter(c => c.startsWith('/v1/projects/')), ['/v1/projects/p/epics?include_retired=true',
  '/v1/projects/p/tasks?kind=feature&include_archived=true&limit=100',
  '/v1/projects/p/tasks?kind=feature&include_archived=true&limit=100&after=2'], 'opening the view: the epics and the feature records, page by page');
// A hub that does not list feature records: the selector lists the epics
// as before, and the view still works.
hub.kindFeature = false;
await open('?project=p&view=features&goal=g');
assert.deepEqual([...element('goalSel').innerHTML.matchAll(/<option value="([^"]*)"/g)].map(m => m[1]), ['', 'old', 'g', 'empty', 'r'], 'the order of creation, no counts');
assert.doesNotMatch(element('goalSel').innerHTML, /optgroup|feature|\(\d/);
assert.match(element('goalSel').innerHTML, />\(retired\) Retired goal<\/option>/, 'the plain selector marks a retired epic in front too');
assert.equal((body().match(/class="frow/g) || []).length, 3);
await open('?project=p&view=features&goal=empty');
assert.equal(body(), '<div class="dim">This epic has no feature records.</div>', 'without counts, no pointer to them');
hub.kindFeature = true;
// A project where no epic has features: the text does not point to goals
// that are not there.
const saved = hub.features;
hub.features = {};
await open('?project=p&view=features&goal=empty');
assert.equal(body(), '<div class="dim">This epic has no feature records. No epic of this project has any yet.</div>');
hub.features = saved;
// A hub that ignores kind lists tasks: none of them is counted as a feature.
hub.ignoresKind = true;
await open('?project=p&view=features&goal=g');
assert.doesNotMatch(element('goalSel').innerHTML, />\(\d+[,)]/, 'a task is not a feature');
hub.ignoresKind = false;
await open('?project=p&view=features');
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

// A confirmed task of another project, opened from a feature: the task page
// moves to its project, so its way back drops this project's goal and feature.
await open('?project=p&view=features&goal=g&feature=f1');
await open('?task=t-other');
assert.equal(run('PROJ'), 'q');
assert.equal(run('listURL()'), '/tasks?project=q&view=features', 'a foreign task leaves the goal and the feature behind');
await open(run('listURL()').slice('/tasks'.length));
assert.equal(run('GOAL'), '');
assert.match(body(), /Choose a goal to see its features\./, "the way back asks for a goal of the task's project");
assert.equal(element('epicDetails').hidden, true, 'no goal panel on the way back');
// The browser's back button from that task returns to the address it came
// from: the first project with its goal and feature.
await open('?project=p&view=features&goal=g&feature=f1');
await open('?task=t-other');
await open('?project=p&view=features&goal=g&feature=f1');
assert.equal(run('PROJ'), 'p');
assert.equal(run('GOAL'), 'g');
assert.equal(run('FEATURE'), 'f1');
assert.match(body(), /<h2>confirmed tasks<\/h2>/, 'back restores the feature');
// A task of the same project keeps them for the way back.
await open('?project=p&view=features&goal=g&feature=f1');
hub.records['t-open'] = {id: 't-open', title: 'Open work', state: 'BLOCKED', project: 'p'};
await open('?task=t-open');
assert.equal(run('listURL()'), '/tasks?project=p&view=features&goal=g&feature=f1', 'a task of the same project keeps the way back');
delete hub.records['t-open'];

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

console.log('PASS features view: the goal details closed by default, the selector with counts, the waits line of an open feature, order and tie, waits-on, another project and an unreadable task, empty views, missing route, the address');
