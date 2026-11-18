package server

// reviewedHoles is the reviewed list of template holes in the console
// pages that the rule of console_scripts_test.go does not allow by itself.
// Each was read where it stands and found to hold nothing an agent or a
// user other than the viewer wrote, unescaped. n is how many times the
// hole occurs on the page: a new occurrence fails the check until it is
// read too. An entry that matches no hole fails as well.
//
// An inline entry stands inside an inline event handler, where the browser
// decodes HTML entities before the handler runs, so esc is no escape
// there; a url entry stands in an address attribute, where esc does not
// stop a javascript: address. Its ground says why the value is safe there
// (task 01a1cf05, comment seq 582).
var reviewedHoles = []holeEntry{
	// admin.html
	{page: "admin.html", ctx: ctxHTML, expr: `e`, n: 1, why: "acts: an event name, a key of the object literal each caller writes"},
	{page: "admin.html", ctx: ctxHTML, expr: `count`, n: 1, why: "treeEntry: the page's template of g.stats.memories a few lines above, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `total`, n: 1, why: "renderFacts: ms.length"},
	{page: "admin.html", ctx: ctxHTML, expr: `rev`, n: 3, why: "recEditor: each caller passes a record's revision from the hub, or 0"},
	{page: "admin.html", ctx: ctxHTML, expr: `kebab`, n: 1, why: "treeEntry: the page's template a few lines above, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `SPINES[i%SPINES.length]`, n: 1, why: "a colour from the page's constant list"},
	{page: "admin.html", ctx: ctxHTML, expr: `chapKebab`, n: 1, why: "treeEntry: the page's template a line above, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `n(counts[c.name])`, n: 1, why: "treeEntry's n: a count of facts from chapCounts, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `n(counts.unfiled)`, n: 1, why: "treeEntry's n: a count of facts from chapCounts, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `org[o]`, n: 1, why: "treeEntry: a count of facts per origin, counted by the page"},
	{page: "admin.html", ctx: ctxHTML, expr: `label`, n: 2, why: "section and bar: each caller passes a literal"},
	{page: "admin.html", ctx: ctxHTML, expr: `count[t]`, n: 1, why: "renderTagBar: a count of facts per label, counted by the page"},
	{page: "admin.html", ctx: ctxHTML, expr: `i`, n: 1, why: "renderFacts: the index of ms.map((m,i)=>…)"},
	{page: "admin.html", ctx: ctxHTML, expr: `chapColor(chs[0])`, n: 1, why: "a colour from the page's constant list, or a fixed CSS variable"},
	{page: "admin.html", ctx: ctxHTML, expr: `h.replace(/\n/g,"<br>")`, n: 1, why: "md2html: h is esc(b) with fixed markup added, lines above"},
	{page: "admin.html", ctx: ctxHTML, expr: `covered`, n: 1, why: "sweepRelabel: a sum of the hub's integer considered counts"},
	{page: "admin.html", ctx: ctxHTML, expr: `found()`, n: 1, why: "sweepRelabel: a sum of array lengths"},
	{page: "admin.html", ctx: ctxHTML, expr: `pool`, n: 1, why: "sweepRelabel: null or the hub's integer unfiled count"},
	{page: "admin.html", ctx: ctxHTML, expr: `up`, n: 1, why: "renderBindings: the page's template with esc(b.model), or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `cls`, n: 3, why: "a CSS class chosen from literals a line above"},
	{page: "admin.html", ctx: ctxHTML, expr: `diff`, n: 1, why: "loadLog: the page's templates with esc, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `r.zy`, n: 1, why: "loadUsage: a count of runs, counted by the page"},
	{page: "admin.html", ctx: ctxHTML, expr: `extra`, n: 1, why: "bar: each caller passes the page's template with esc and the hub's numbers"},
	{page: "admin.html", ctx: ctxHTML, expr: `pct`, n: 1, why: "bar: a percentage from /v1/health's resources"},
	{page: "admin.html", ctx: ctxHTML, expr: `h.projects`, n: 1, why: "/v1/health: len(ids), an integer (server.go)"},
	{page: "admin.html", ctx: ctxHTML, expr: `del`, n: 1, why: "loadDocs: fixed markup, or \"\""},
	{page: "admin.html", ctx: ctxHTML, expr: `m`, n: 1, why: "modeBtn: each caller passes a literal"},
	{page: "admin.html", ctx: ctxHTML, expr: `indent`, n: 2, why: "colWikiHTML: a constant attribute string"},
	{page: "admin.html", ctx: ctxHTML, expr: `inner`, n: 1, why: "colWikiHTML: the page's own templates and render's results"},
	{page: "admin.html", ctx: ctxHTML, expr: `note`, n: 1, why: "recEditor: each caller passes \"\" or the page's template with esc"},

	// tasks.html
	{page: "tasks.html", ctx: ctxHTML, expr: `COUNTS.counts[s]`, n: 1, why: "a task count per state from the hub"},
	{page: "tasks.html", ctx: ctxHTML, expr: `s`, n: 4, why: "a state from the page's constant STATES (or '' for all)"},
	{page: "tasks.html", ctx: ctxHTML, expr: `n(s)`, n: 1, why: "stateChipsHTML's n: the page's template of counts, or \"\""},
	{page: "tasks.html", ctx: ctxInline, expr: `s`, n: 1, ground: "fixed enum", why: "a state from STATES, in FILTER.state='…'"},
	{page: "tasks.html", ctx: ctxInline, expr: `taskURL(t.id)`, n: 2, ground: "hub-assigned id", why: "go('…') of a task row or card"},
	{page: "tasks.html", ctx: ctxHTML, expr: `st`, n: 3, why: "a state from the page's constant STATES"},
	{page: "tasks.html", ctx: ctxInline, expr: `st`, n: 1, ground: "fixed enum", why: "a state from STATES, in dropCard(event,'…')"},
	{page: "tasks.html", ctx: ctxHTML, expr: `st.replace("_"," ")`, n: 1, why: "a state from the page's constant STATES"},
	{page: "tasks.html", ctx: ctxInline, expr: `esc(t.id)`, n: 2, ground: "hub-assigned id", why: "dragStart and moveTask of a card"},
	{page: "tasks.html", ctx: ctxHTML, expr: `BOARD_CAN`, n: 1, why: "a boolean the page sets"},
	{page: "tasks.html", ctx: ctxHTML, expr: `prefix`, n: 14, why: "contentForm: each caller passes \"new\" or \"ed\""},
	{page: "tasks.html", ctx: ctxHTML, expr: `label`, n: 2, why: "fieldHTML and refsHTML: each caller passes a literal"},
	{page: "tasks.html", ctx: ctxInline, expr: `taskURL(ref)`, n: 1, ground: "gated ref", why: "refHTML links a ref only when it matches /^[0-9a-f-]{36}$/"},
	{page: "tasks.html", ctx: ctxURL, expr: `esc(ref)`, n: 1, ground: "scheme gate", why: "refHTML links a ref only when it starts with http:// or https://"},
	{page: "tasks.html", ctx: ctxHTML, expr: `body`, n: 1, why: "refHTML: the page's template with esc and taskURL, or esc(ref)"},
	{page: "tasks.html", ctx: ctxHTML, expr: `kind`, n: 1, why: "refHTML: esc(r.kind) a few lines above"},
	{page: "tasks.html", ctx: ctxHTML, expr: `note`, n: 1, why: "refHTML: the page's template with esc(r.note), or \"\""},
	{page: "tasks.html", ctx: ctxHTML, expr: `scope`, n: 1, why: "refHTML: the page's template with esc(r.scope), or \"\""},
	{page: "tasks.html", ctx: ctxHTML, expr: `ro`, n: 1, why: "renderTask: fixed markup, or \"\""},
	{page: "tasks.html", ctx: ctxInline, expr: `taskURL(d)`, n: 1, ground: "dependency id", why: "go('…') of a dependency"},
	{page: "tasks.html", ctx: ctxHTML, expr: `mine`, n: 1, why: "showConflict: the revision number of the task the page loaded"},
	{page: "tasks.html", ctx: ctxInline, expr: `esc(JSON.stringify(attempt))`, n: 1, ground: "json literal", why: "reapply(…) of the viewer's own unsaved edit"},
	{page: "tasks.html", ctx: ctxInline, expr: `esc(c.id)`, n: 1, ground: "hub-assigned id", why: "the copy link of a comment"},
}
