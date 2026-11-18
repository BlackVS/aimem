# Documents kept on the hub

Status: proposal for the owner's review of the goal-driven process
(2026-11-22). Nothing in it is implemented, and no task that follows from
it is started before the owner decides. Section 12 lists his decisions.

## 1. Why

On 2026-11-18 the owner asked for a place on the hub where text documents
such as the feature guide are kept, shown in the web console and edited
there without rebuilding the binary. Today such a text changes only with
a release, and a closed network gets a release only through an aiforge
export (docs/ADMIN-MANUAL.md), so even a one-line correction waits for the
next release and the next export.

His further direction, which this document designs for:

- **The hub is the place of record, not the binary.** The built-in text is
  at most the seed a hub starts from.
- **A real store with revisions,** a local git repository or the database,
  not plain files.
- **Two levels.** Hub-level documents are for every project, and only the
  human operator edits them. A project takes the hub-level document as its
  default and may then keep its own version. A project changes only its
  own documents.
- **Agents do not cut these documents up as they please.** A project's
  version is edited by the operator, or by an agent at his request,
  probably only his own session or the architect's with his permission.

## 2. What exists today

Each claim names the place to check it.

**Texts built into the binary.**

- `docs/embed.go` embeds the team playbooks and request templates
  (`TeamGuidance`), the rule for kept text (`WritingRule`) and the feature
  guide (`FeatureGuide`).
- `team_context` serves the team guidance (`internal/mcp/guidance.go`, from
  `internal/teamguide`), and with `section=features` the feature guide
  (`internal/builtintext`). `writing_rule` serves the rule
  (`internal/writingrule`). `aimem teams context` prints the same texts.
- Each answer has a header with its unit, the build version, a SHA-256
  digest and its source. The feature guide's says "source: built in,
  docs/FEATURES.md as committed in this aimem build".
- They need no hub, credential or network, they match the tools of the
  binary that serves them, and they change only with a release.

**The process set.**

- Files in a git repository: a manifest, a handbook, checklists and
  templates (`internal/process`, the reference set at `processes/aimem`).
- An administrator selects a set for a project at a pinned commit
  (`aimem process select`). The hub records the selection in the project's
  `process_history` and serves the set (`GET /v1/projects/{p}/process/set`).
- `process_context` gives a session the selected set
  (`internal/mcp/process_context.go`). A change is a commit, a review and a
  new selection; the console does not edit a set.

**Shared documents.**

- Each project, the personal store (`user`) and each knowledge group
  (`group-*`) has its own journal. Its documents are the tables `docs` and
  `doc_revisions` (`internal/store/store.go`, migration v8;
  docs/DESIGN-shared-docs.md).
- Writes are compare-and-swap on a revision, with a history and a
  three-way merge on conflict (`internal/server/docs.go`, `internal/diff3`,
  docs/DESIGN-doc-collab.md). The routes are `/v1/projects/{p}/docs` and
  `/docs/{name}` with `log`, `merge`, `PUT` and `DELETE`
  (`internal/server/server.go`).
- Sessions reach them with `list_docs`, `read_doc` and `update_doc`. Their
  `scope` is a string, `project` or `group:<name>`.
- The console's document view (`admin.html`, `loadDocs`, `openDoc`) shows
  and edits documents of a scope it already lists. It cannot create a
  scope.
- Documents do not travel between hubs. Hub sync streams events, memories
  and group configuration (`/v1/sync/*`), and DESIGN-hub-sync keeps
  documents out of it ("not a second transport").

**Who the hub can tell apart** (`internal/access/store.go`,
`project_grants.go`, `team_profiles.go`, `internal/server/knowledge.go`):

- **The administrator credential** (the environment token or a named admin
  token). The console signs in with it. Admin routes require it.
- **Legacy writer tokens,** which write documents as the administrator does
  (`internal/server/knowledge.go`).
- **A user's tokens.** Each has a scope (`user`, `project` or
  `read-only`), an optional project instance, a label and an expiry. A
  user reaches a project through a grant, `read` or `write`, held by the
  user or one of the user's groups.
- **Team profiles** (aicrew's members), with the team context's role. In
  team mode a member writes only `report-` documents under its grant.
- **What it cannot tell.** Every session that uses one user's tokens is the
  same user to the hub. On the aimem board today the operator's own
  sessions, the architect's session and a worker's session all write as one
  user. The hub cannot tell the person from the agents, or one agent from
  another, except by which token was used.

So the place the owner asks for exists per project and per group, without
the two levels, without a seed, and without a right that separates a
person from an agent.

## 3. The place of record and the seed

**Proposal.**

- **The hub's store holds the text of record.** The binary keeps each
  built-in text only as its *seed*: the text a hub starts from.
- **Seeding happens when the hub starts.** For each seeded document that
  the hub level does not hold yet, the hub writes revision 1 from the
  binary's seed, by the writer `seed vX.Y.Z`. A fresh hub on a closed
  network is seeded by the release it was installed from, with nothing
  else at hand. An upgrade seeds documents that are new in the release.
- **A hub that is not seeded yet,** such as one being upgraded to the
  release that brings the store, answers a read from the binary's seed,
  and the header says "built in, not yet on this hub". A read never fails
  for want of a seed.

**Rejected:**

- *Seeding from the release's files at install,* by the installer. It
  would put document handling into `install-hub.sh` and the Windows
  installer, a sensitive surface, for what the hub can do itself on
  start. It also leaves an upgrade without an installer run unseeded.
- *No seed: an empty hub until the operator writes.* A closed network's
  first sessions would read nothing, and the feature guide is needed on a
  project's first day (task 01a1ed23).

## 4. The store: the database, not a local git repository

**Proposal: the hub's database,** as a hub-level scope of the shared
documents (section 5). It already has everything the owner asks of a store:

- revisions with compare-and-swap;
- a history (`log`);
- a three-way merge on conflict (`merge`, `internal/diff3`);
- a tombstone for retiring a document;
- the console's editor;
- the hub's backup, since the journals are in the hub's state directory.

**Rejected: a local git repository on the hub.** What it would add:

- branches;
- the tools a person uses to read a history;
- signed commits.

What it would require:

- a `git` binary on the hub's host, which the hub does not need today;
- a repository kept beside the state directory, with its own backup;
- a lock around every write, because concurrent console and agent edits
  would race in the working tree;
- a merge that is not the one the console already offers.

Branches and signing are not among the owner's needs, and a review before
an edit is what the process set in git already provides (section 10).

## 5. Two levels

**The hub level.**

- A reserved journal beside `user` and `group-*`, so that no project can
  take its id. This document calls it the hub scope and leaves the
  spelling to the store task.
- Every authenticated token reads its current revisions, including the
  scoped user tokens of every project. That is a new rule: the knowledge
  read check refuses every reserved scope to a scoped token today.
- Only the administrator credential writes it (section 7).

**The project level.**

- A project's own version of a hub-level document lives in the project's
  journal, under the same name, with the shared documents' revisions.
- With every revision the project keeps the hub-level revision it was
  based on, so that the hub and the project can be compared and merged
  later.

**What each document is.** A hub-level document has one of two kinds,
fixed when it is created:

- **Reference:** what the tools do and how to use them, such as the feature
  guide. It has no project versions: every project reads the hub's, which
  follows the hub and its release. A project version of a reference would
  describe the tools wrongly after the next upgrade.
- **Template:** a text a project may make its own, such as a process's
  rules (section 10). It may have project versions.

## 6. A template's copy: at the first edit

**Proposal: the copy is made at the first edit.**

- Until a project changes a template, its sessions read the hub-level
  document directly. A correction at the hub level reaches every project
  that never changed it, the same minute.
- The project's own version comes into being with its first edit, based on
  the hub-level revision it was edited from.
- The console shows, for each project and template, either "uses the hub's
  document" or "has its own version". When the hub's document has changed
  since the project's version was made, it adds "the hub's has changed
  since (revision N to M)" and offers the three-way merge: the hub's
  revision N as the base, the hub's revision M and the project's version.
- A project created before the feature needs nothing: it uses the hub's
  documents until it edits one.
- Retiring a project's version returns the project to the hub's document.

**Rejected: a copy at the project's creation.**

- Every project would hold a copy from its first day, and a later
  correction at the hub level would reach no existing project.
- Projects created before the feature would need a migration to get their
  copies.
- This is the shape the owner first described. The architect recommended
  the copy at the first edit, and this document agrees, with the
  reference/template distinction of section 5.

## 7. Who may edit, and how the hub tells

**Proposal.**

- **The hub level: the administrator credential only,** which in practice
  is the operator in the console. An agent never holds that credential.
  Legacy writer tokens, which write other documents as the administrator
  does, are refused here.
- **A project's version: a token with a new right, "documents",** in
  addition to a write grant on the project. The right belongs to a token,
  not to a user: all of one operator's sessions are the same user
  (section 2), so only the token can separate the session he permits from
  the others.
  - The operator issues such a token in the console for the session he
    permits: his own session, or the architect's.
  - The token's expiry bounds the permission. A permission for one edit is
    a token that expires the same day; a standing one is a longer expiry.
  - The console can revoke it at once.
- **Team members never edit documents** beyond today's `report-`
  documents.
- **A refused write** is answered with a code of its own, for example
  `document_edit_forbidden`. The message names the right and says "ask the
  operator to issue this session a token with the documents right", so
  the agent reports the request instead of retrying.

**Rejected:**

- *The write grant alone.* Every session with a write grant on the project
  could edit, which is what the owner wants to prevent.
- *A person's approval of each edit,* as a queue in the console. It keeps
  a person's hand on every change, but makes each edit wait for the
  operator. If the owner wants that strictness, it can be added on top of
  the documents right later.
- *Telling a person from an agent by the client.* A browser and an MCP
  client can both send any header; only the credential is checked.

## 8. A release under an edited text

**Proposal.**

- When a release brings a newer seed and the hub-level document still
  holds an earlier seed unchanged, the hub writes the new seed as the next
  revision, by `seed vX.Y.Z`.
- When the operator has edited it, the hub keeps his version and records
  that the seed changed. The console marks the document "the release's
  text has changed since your edit" and offers the three-way merge: the
  seed his edit was based on, the new seed, and his version.
- A session reading a reference that is out of date with its release gets
  a header that says so, so that a session can tell when the guide may
  describe older tools.

**Rejected:**

- *The new seed replaces the edit.* It loses the operator's change without
  a word.
- *The edit silently wins.* A reference that describes an older release's
  tools misleads every session, and nobody is told.

## 9. What a session reads

**Proposal.**

- **Order:** the project's version when it has one (templates only), else
  the hub level, else the binary's seed on a hub not yet seeded.
- **The header names which one answered and at which revision:** "source:
  project P, revision N, based on hub revision M", "source: hub, revision
  N", or "source: built in, not yet on this hub". The header of today's
  built-in read already says "source: built in"; this extends it.
- **The reads, with no new listed tool:**
  - A reference is read where it is read today: the feature guide with
    `team_context section=features`.
  - A template, or a hub document without a seed, is read with `read_doc`
    and the existing `scope` argument: a new value `hub`, and for a
    project the project scope, which answers the hub's document when the
    project has no version. `scope` is already a string, so its schema
    does not change; the descriptions of `read_doc` and `list_docs` do.
- **The local facade's network.** `team_context` needs no network today.
  With this proposal the local facade asks the hub for the hub's version
  when it has one configured, and answers with the binary's seed, saying
  so in the header, when the hub cannot be reached.

**Rejected:**

- *A new listed tool,* such as `hub_doc`. It costs every session's listed
  bytes, while `team_context` and `read_doc` already carry the reads.
- *A copy in each project's documents made by hand.* It differs from the
  hub's and the release's, which task 01a1ed23 ruled out.

**The built-in feature guide just merged** (task 01a1ed23) stays as the
seed and as the fallback when the hub cannot answer.
`docs/FEATURES.md` remains the source of the seed and is still reviewed
with the code it describes. Once the store exists, the hub's document is
the text of record.

**What the console shows.** A "Documents" view at the hub level lists each
document with its kind, its source (seed or edited, revision, writer,
time) and any out-of-date mark. Per project it shows each template as
"uses the hub's document" or "has its own version". The operator reads,
edits, merges, retires and creates documents there, with the existing
history. Issuing a documents token belongs to the access view.

## 10. Where a process's rules live: the owner's choice

The trial keeps the goal-driven process's rules (the tiers, the support
budget, the course check) in a proposal document, and they change every
few days. The owner's thoughts lean to the hub's store with a person's
hand on it. This document sets out both sides and does not choose.

**Side A: the process set in git, reviewed and pinned.**

- A rule is a file in the set's repository. A change is a commit, reviewed
  like code, and a new selection pins it for each project
  (`aimem process select`).
- Every session of a project reads the same reviewed text, and
  `process_history` records which commit applied when. Which rule a task
  was judged by can be answered afterwards.
- A closed network gets the set from a reachable repository or an archive
  import.
- **Cost during the trial:** each change is an edit, a review, a merge and
  a selection per project, about a day per change while the rules change
  every few days. Sessions run under the old rule in between. The
  operator needs git and the repository.

**Side B: a template on the hub, edited in the console.**

- The rules are a hub-level template (sections 5 and 6). An edit reaches
  every project that has not made its own version, the same minute, and a
  project may keep its own variant.
- The history of revisions says who changed what and when, and only the
  operator or a session he permits can change it (section 7).
- **Cost:**
  - Nothing reviews an edit before it applies.
  - Which rule a task was judged by is found only by matching the task's
    times to the document's revisions.
  - The rules stay on one hub (section 11).
  - The process set's selection no longer pins them.

**The third path:** side B during the trial, while the rules move, and
side A once they settle, with the move as an explicit step. It buys speed
now for a migration later.

**What each side means for the goal's feature** "The process set and the
pickup policy carry the goal-driven process": with side A the feature puts
the rules into the process set; with side B it becomes "a hub template
carries the rules, and the process set points to it".

## 11. Peers

**Proposal: a document belongs to its hub.** Hub sync does not carry
documents, by design (DESIGN-hub-sync), and two hubs that each edit one
would need a merge across hubs that nothing provides. Each hub is seeded
by its own release. An operator who wants the same edited text on two hubs
edits it on both, or carries it with an export and import (section 13).

**Rejected: documents travel with hub sync.** That makes them a second
transport, against the sync design, and needs a merge across hubs for
every edit.

## 12. What the owner decides

1. **The place of record:** the hub's database, with the binary's text as
   the seed (sections 3 and 4).
2. **The two kinds:** references follow the hub and have no project
   versions; templates may have them (section 5).
3. **A template's copy:** at the first edit, not at the project's creation
   (section 6).
4. **Who edits a project's version:** a token with the documents right,
   issued by the operator per session with an expiry (section 7); or the
   stricter approval of each edit.
5. **Where a process's rules live:** side A, side B or the third path
   (section 10).
6. **Peers:** each hub keeps its own documents (section 11).

## 13. The work that would follow

None of it is promoted before the owner decides. Sizes and surfaces as the
project's rules name them; the sensitive ones are the HTTP API and
authentication, the store's schemas and migrations, and the tool sets.

| task | size | surface |
| --- | --- | --- |
| The hub scope: a reserved journal, a document's kind, the seed on start and the based-on revisions | M | store schema and migration, sensitive |
| The read rule: every authenticated token reads the hub scope; the administrator writes it | S | authentication, sensitive |
| The documents right on a token, issued and revoked in the console; the refusal code | M | authentication and access schema, sensitive |
| Project versions of templates: the copy at the first edit, the based-on revision, the merge | M | HTTP API, sensitive |
| The reads: `team_context` and `read_doc` answer by the order of section 9, with the header and the fallback; descriptions | M | tool sets and the local facade's network, sensitive |
| The console's "Documents" view: kinds, sources, out-of-date marks, project versions, merge, history | M | product, ordinary |
| A release's newer seed: written when unchanged, marked and merged when edited | S | store, sensitive |
| Export and import of documents between hubs | S | HTTP API, sensitive; optional |
| Side B or the third path of section 10: the rules as a hub template, the process set pointing to it | S | process set content, ordinary; only if chosen |

**A closed network needs the first two tasks and the reads** for its
sessions to read the hub's documents. The console view is what lets its
operator correct a document without a release. Until then the built-in
texts serve it as today, since the seed is in the binary it already has.
