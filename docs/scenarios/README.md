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

## Cards

| Card | Path it covers | Tier |
| --- | --- | --- |
| [00-fixture](00-fixture.md) | The hub and workstation answer; a skills release and a process set by tag on the hub; the workstation adds the hub and reads its skills over TLS | on demand |

Tiers: **release gate** cards must pass before a release's prepare pull
request is marked ready; **pilot** cards cover a path a pilot site uses;
**on demand** cards check the suite itself or a path run when it changes.
The scenario job runs every card on every change, whatever its tier.

Planned cards, one task each:

- 01, the new-project path of QUICKSTART-NEW-PROJECT, sections 2 and 3,
  with the user-token path and the project-local token path (release gate).
- Upgrade of a wired workstation and of the hub, run from a local binary
  directory with `AIMEM_PREBUILT_DIR`.
- Process re-select by tag with `--expect`, and the hub default.
- Token lifecycle as seen from the workstation: expiry, revocation and the
  replacement.
- TEAM-QUICKSTART with aicrew, in the aicrew repository.
- A second hub as a peer.
- A closed-network install from the release archive.

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
  which machine runs the command.
- **The reason line** follows the heading: one line on why the step
  exists. A step has exactly one.
- **The `sh` block** holds exactly one `aimem` command. A long command may
  continue on the next line after a trailing backslash.
- **The `text` block** follows the `sh` block. Each non-blank line in it
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
(`$NAME`), quoting (`"`, `'`), globs (`*`, `?`), substitutions, a
home directory (`~` at the start of a word) or a comment (`#` at the
start of a word). The parser refuses such a step, so a card never shows a person a command that
the runner did not run. When a path needs a value that a shell would
supply, it becomes a placeholder.

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
| `<hub-name>` | The name the workstation gives the hub: `team`. |
| `<project>` | The project the scenario works on: `scenario-project`. |
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
