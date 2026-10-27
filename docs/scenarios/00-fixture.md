# 00. The scenario fixture answers

This card checks that the two machines every scenario runs on are working:
a hub serving its API over TLS and a workstation with its own local
service. It also exercises the two inputs a hub administrator brings from
outside, a process set selected by tag from a Git repository and a skills
release archive, and connects the workstation to the hub. If this card
fails, every other card would fail for a reason that has nothing to do
with its own path, so it is the first thing to read.

Tier: on demand. The scenario suite runs it in CI on every change.

## Preconditions

- A hub machine with `aimem serve` running and its HTTPS listener on
  `<hub-url>`, with a certificate signed by the CA in `<ca-file>`.
- A workstation with its own `aimem serve` running.
- A writer token for the workstation in `<token-file>`, readable only by
  your account (on the hub: `aimem token add <machine>`).
- A skills release archive at `<skills-archive>`, version
  `<skills-version>`, with its `SHA256SUMS` beside it.
- The process repository `https://github.com/BlackVS/aimem.git` with the
  tag `<tag>`.

## Steps

### 1. The hub's service answers (hub)

Every hub step below talks to this service through its local socket.

```sh
aimem health
```

```text
"status":"ok"
```

### 2. The workstation's service answers (workstation)

The workstation keeps its own state; nothing below reaches the hub until step 7.

```sh
aimem health
```

```text
"status":"ok"
```

### 3. Tasks are switched on for the project (hub)

A project with tasks on but no process selected is reported as not ready.

```sh
aimem tasks on -p <project>
```

```text
tasks on for project "<project>"
not ready for work: tasks on, process not selected
```

### 4. The skills release is imported (hub)

The process set requires a skill, and the hub serves only skills it has imported.

```sh
aimem skills import --from-archive <skills-archive>
```

```text
importing release set <skills-version>
"imported":true,"source":"release"
```

### 5. The process set is selected by tag (hub)

A tag resolves to one commit, and the hub records the repository, tag and commit.

```sh
aimem process select https://github.com/BlackVS/aimem.git <tag> -p <project>
```

```text
<tag> holds commit
"repo":"https://github.com/BlackVS/aimem.git","ref":"<tag>"
```

### 6. The project shows its process set (hub)

This is what a session on the project is told about its process.

```sh
aimem process show -p <project>
```

```text
Process set: aimem <tag> @
Required skills: oh-code-review
```

### 7. The workstation adds the hub (workstation)

The hub's certificate is verified against the CA, and the token is read from a file.

```sh
aimem hub add <hub-name> <hub-url> --token-file <token-file> --ca-file <ca-file>
```

```text
hub "<hub-name>" configured
```

### 8. The workstation reads the hub's skills over TLS (workstation)

This request carries the writer token to the hub over the verified TLS connection.

```sh
aimem skills list -p <project>
```

```text
release set: <skills-version>
oh-code-review <skills-version> (release
```
