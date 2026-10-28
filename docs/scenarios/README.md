# Scenario cards

A scenario card is a documented usage path written as numbered steps that
a person can follow by hand on real machines and that the scenario suite
runs against the real `aimem` binary. Each card is both at once. The suite
reads the commands and the expected output from the card itself, so a
change to the program or to the card that makes them disagree fails CI.

The suite lives in `internal/scenario` under the build tag `scenario`:

```sh
go test -tags scenario -count=1 ./internal/scenario/...
```

It builds the binary once per run. For every card it starts a fresh hub (the
binary as `aimem serve` with its HTTPS listener on loopback and a
certificate from a test CA) and a fresh workstation (its own `aimem serve`
and state root), each with its own home directory and none of the
caller's `AIMEM_` settings. It then runs the card's steps in order. The CI
job `scenario` runs it on Linux and Windows.

The machines do not inherit the caller's `PATH`. On Linux and macOS it is
the directory of the binary under test, which also holds a link to the
`git` the suite found. On Windows it is that directory, the `cmd`
directory of the git installation, `System32` and the Windows directory.
No agent client is on it, so a command that looks a client up on `PATH`
finds none on any machine. A card names the tool it installs for, as in
`aimem skills install --tool claude`, and a card cannot pass because of a
client installed on the machine running the suite.

## Cards

| Card | Path it covers | Tier |
| --- | --- | --- |
| [00-fixture](00-fixture.md) | The hub and workstation answer; a skills release and a process set by tag on the hub; the workstation adds the hub and reads its skills over TLS | on demand |
| [01-new-project](01-new-project.md) | QUICKSTART-NEW-PROJECT, sections 2 and 3, with a user task token: the hub's board, process, user, grant and token; the workstation's wiring, hub, task credential and checks; a task created and listed through the API; a second project that needs only a grant | release gate |
| [02-new-project-project-token](02-new-project-project-token.md) | The same path with a project-local task token (TASK-CREDENTIALS.md, "Project-local override"): `token-issue --project` on the hub, `task-token set` in the checkout, the token writes to its project and is refused for another | release gate |

Tiers: **release gate** cards must pass before a release's prepare pull
request is marked ready (DEVELOPMENT.md, "Release gate"); **pilot** cards
cover a path a pilot site uses; **on demand** cards check the suite itself
or a path run when it changes. The scenario job runs every card on every
change, whatever its tier.

Planned cards, one task each, in this order:

1. 03, upgrade of a wired workstation and of the hub, run from a local
   binary directory with `AIMEM_PREBUILT_DIR` and without the network.
2. 04, process re-select by tag with `--expect`, and the hub default.
3. 05, token lifecycle as seen from the workstation: expiry, revocation
   and the replacement.
4. TEAM-QUICKSTART with aicrew, in the aicrew repository.
5. 06, a second hub as a peer.
6. 07, a closed-network install from the release archive.

## Writing a card

A card is `NN-slug.md` in this directory: a two-digit number, then a
lowercase slug. It starts with a level-one heading and a paragraph saying
what the scenario does and why it matters, then a `## Preconditions` list
of what a person needs before step 1, then `## Steps`.

Each step has this shape:

````markdown
### 3. Tasks are switched on for the project (hub)

A project with tasks on but no process selected is reported as not ready.

```sh
aimem tasks on -p <project>
```

```text
tasks on for project "<project>"
not ready for work: tasks on, process not selected
```
````

- **The heading** is `### N. <title> (hub)` or `### N. <title>
  (workstation)`. Steps are numbered from 1 without gaps. The side says
  which machine runs the command. Workstation commands run in the
  project's directory, hub commands as the hub's service account.
- **The reason line** follows the heading: one line on why the step
  exists. A step has exactly one.
- **The `sh` block** holds exactly one `aimem` command. A long command may
  continue on the next line after a trailing backslash. A step that sends
  a request to the hub's API has an `http` block in its place (see
  "HTTP steps" below).
- **The `text` block** follows the `sh` or `http` block. Each non-blank line in it
  is one expected line. It must appear in the command's combined standard
  output and standard error as a substring, so a line can quote the
  stable part of a longer output line. A step needs at least one expected
  line. Copy the lines from the real output; do not paraphrase.
- **The command must succeed.** A command that exits with a non-zero
  status fails the step.

### No shell

The runner does not use a shell. It splits the command into words at
spaces and runs the binary with those words as its arguments. A card
therefore cannot contain anything a shell would interpret: no pipes
(`|`), redirections (`>`, `<`), command separators (`;`, `&`), variables
(`$NAME`), quoting (`"`, `'`), escapes (a backslash anywhere but the
end of a line, where it continues the command), globs (`*`, `?`),
substitutions, a home directory (`~` at the start of a word) or a comment
(`#` at the start of a word). The parser refuses such a step, so a card
never shows a person a command that the runner did not run. The check
reads the card's own words, not the placeholders' values, so a Windows
path substituted for a placeholder may hold backslashes. When a path needs a value that a shell would
supply, it becomes a placeholder.

### HTTP steps

Some paths end in the hub's API, which an agent's task tools call. A
step that checks one sends the request itself, from an `http` block:

````markdown
```http
POST /v1/projects/<project>/tasks
Token-File: <task-token-file>
Idempotency-Key: scenario-01-first-task

{"title": "First task from the workstation", "state": "BACKLOG"}
```
````

- **The first line** is the method (`GET`, `POST`, `PUT`, `PATCH` or
  `DELETE`) and a path under `/v1/`. The runner sends it to `<hub-url>`
  over TLS verified against `<ca-file>`.
- **`Token-File`** names, as one placeholder, the file whose token is sent
  as `Authorization: Bearer`. It is required and is not sent itself; an
  `Authorization` line is refused, so no secret is written in a card.
- **Other lines** up to a blank line are headers, `Name: value`.
- **The body**, after the blank line, is JSON; a request without one ends
  at the headers.
- **The output** the expected lines are matched against is the status
  line, such as `201 Created`, then the response body. A status other
  than 2xx fails the step, as a non-zero exit does.

A person sends the same request with curl, the token read from the file:

```text
curl --cacert <ca-file> -H "Authorization: Bearer $(cat <task-token-file>)" \
  -H 'Idempotency-Key: scenario-01-first-task' -H 'Content-Type: application/json' \
  -X POST '<hub-url>/v1/projects/<project>/tasks' \
  -d '{"title": "First task from the workstation", "state": "BACKLOG"}'
```

So that this curl form is exact, the parser refuses a single quote, `$`,
a backquote or a backslash in the block's own text.

### Placeholders

A placeholder is a lowercase name in angle brackets. The runner
substitutes it in commands and in expected lines; a placeholder the list
below does not name fails the parse. A person following the card by hand
replaces each one with the value on their own machines.

| Placeholder | The value |
| --- | --- |
| `<hub-url>` | The hub's HTTPS URL, `https://127.0.0.1:<port>` in the suite. |
| `<ca-file>` | A PEM file holding the CA that signed the hub's certificate. |
| `<token-file>` | A file holding a writer token issued on the hub with `aimem token add`, readable only by its owner. |
| `<task-token-file>` | A file for the user task token: `aimem access token-issue-user --output` writes it on the hub, and the workstation reads it. |
| `<project-token-file>` | A file for a project-scoped task token: `aimem access token-issue --output` writes it on the hub, and the workstation reads it. |
| `<hub-name>` | The name the workstation gives the hub: `team`. |
| `<project>` | The project the scenario works on: `scenario-project`. |
| `<project-2>` | A second project: `scenario-project-2`. |
| `<machine>` | The label of the workstation's credentials: `scenario-machine`. |
| `<user>` | The access user the task board knows: `scenario-agent`. |
| `<repo-url>` | The project's repository URL, which the hub records without contacting it: `https://github.com/example/scenario-project.git`. |
| `<expires>` | A token expiry, 30 days after the run, in RFC 3339 (`2027-01-01T00:00:00Z`). |
| `<tag>` | The process repository's tag the scenario selects. |
| `<skills-archive>` | A skills release archive, with its `SHA256SUMS` beside it. |
| `<skills-version>` | That archive's version. |

The process repository URL is printed as written,
`https://github.com/BlackVS/aimem.git`. In the suite, Git maps that URL to
a local repository holding this checkout's `processes/` directory at
`<tag>`, so the suite needs no network.

### Failures

A failed step names the card file and line, the step and its title, then
prints the command's output:

```text
docs/scenarios/00-fixture.md:61: step 3 (Tasks are switched on for the project): expected line "not ready for work: tasks on, process not selected" not in output:
```

The line is the expected line that did not match, or the command's line
when the command failed. A card that the parser refuses names its file and
line in the same way.

### When to add a card

A documented usage path that is added or changed gets a card, or a change
to the card that covers it, in the same pull request. The commands a card
names are also checked by the documentation examples check
(`scripts/check-doc-examples.sh`).
