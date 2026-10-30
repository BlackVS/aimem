# Quickstart

For project task boards and agent task access, see the
[Kanban quickstart](KANBAN-QUICKSTART.md). To add one new project on a
fresh workstation to a hub that already runs, with tasks, the session
handoff and design documents, follow
[the new-project quickstart](QUICKSTART-NEW-PROJECT.md).

Fifteen minutes from nothing to a hub your agents share. Three stages,
each useful on its own — stop after any of them.

- **Stage 1** (2 min) — one machine, journaling and surviving compaction.
- **Stage 2** (10 min) — a hub, so several machines share one memory.
- **Stage 3** (3 min) — turn on curation, so raw turns become facts.

You need: a project directory, and for stage 2 a small always-on Debian
or Ubuntu host (an LXC container or VM with 1 vCPU and 1 GB RAM is
plenty).

---

## Stage 1 — one machine

Run this **inside a project directory**:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.20/boot.sh | bash
```

On Windows, in a PowerShell window (from cmd, type `powershell` first),
download the release's boot script, check it against the release's
`SHA256SUMS` and run it
([README](../README.md#install) says why there is no one-line form):

```powershell
& {
  $ErrorActionPreference = 'Stop'
  $d = New-Item -ItemType Directory -Force (Join-Path $env:TEMP 'aimem-boot')
  $f = Join-Path $d 'boot.ps1'; $s = Join-Path $d 'SHA256SUMS'
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.20/boot.ps1 -OutFile $f -UseBasicParsing
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.20/SHA256SUMS -OutFile $s -UseBasicParsing
  $want = ((Select-String -LiteralPath $s -Pattern '\sboot\.ps1$').Line -split '\s+')[0]
  if ((Get-FileHash -LiteralPath $f).Hash -ne $want) { throw 'boot.ps1 does not match SHA256SUMS' }
  Unblock-File -LiteralPath $f
  powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
}
```

**Restart your Claude Code or OpenCode session** — hooks and plugins load
at startup, so a session already running will not journal.

The one-liner is for this first install. Every later release installs
with `aimem upgrade`, from any directory: it checks the release against
its SHA256SUMS and rolls back if the new release does not come up
([INSTALL-CLIENT.md, Upgrading](INSTALL-CLIENT.md#upgrading)).

Now work normally for a few turns, then list the project's sessions and
read one of them by its `session_id`:

```sh
aimem sessions -p "$(aimem project-id .)"
aimem timeline -p "$(aimem project-id .)" -s <session-id>
```

Every turn of that session is there, oldest first: request, reply,
tools, files touched. Nothing you did required an LLM call or a network
round trip.

### What just became true

- A crash or `/compact` no longer loses the thread. `docs/SESSION-STATE.md`
  is re-injected into the agent's context at session start, and the full
  journal is a query away.
- Your agent can search its own history:
  ```sh
  aimem search -p "$(aimem project-id .)" -q "retry logic"
  ```
- Secrets are stripped before anything is written, so the journal is safe
  to keep.

### Make the handoff work for you

`docs/SESSION-STATE.md` was created in your project. It is the one file
the agent reads first every session. Tell your agent to keep it current —
`AGENTS.md` (also just created) already carries the protocol. The habit
that matters: **verify, then write**. A handoff that records intentions
instead of verified results is worse than none.

Try it: end a session, start a new one, and ask "where did we leave off?"

---

## Stage 2 — a hub

A hub merges journals and memory from every machine. Until you have one,
each machine remembers only itself.

### 2.1 Install it

On the hub host, **as root**:

```sh
AIMEM_HUB_NAME=home \
AIMEM_DOMAIN=hub.example.com \
bash <(curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.20/install-hub.sh)
```

Substitute your own hostname. `AIMEM_DOMAIN` generates a self-signed
certificate and serves TLS immediately; `AIMEM_HUB_NAME` labels the
console, which matters the moment you run a second hub.

The hub's **admin** credential is the env token, `AIMEM_HTTP_TOKEN` in
`~/.config/aimem/env` on the hub: the one you paste into the console,
and the one that can change configuration. The installer never prints
it, and it stays on the hub. At the end the installer prints how to
give each workstation its own token, and for its self-signed
certificate a `--pin` value.

### 2.2 Check it is up

```sh
curl -sk https://hub.example.com:8440/v1/status
```

That endpoint is public on purpose and says only whether the hub is
alive, which build it runs, and for how long. Everything else needs the
token.

### 2.3 Point your workstation at it

First mint the workstation its **own token**, on the hub (as the
service user). A *writer* token covers everything a workstation does —
events, sync, recall, shared documents — but not hub administration,
and revoking it later touches only that one machine:

```sh
aimem token add my-laptop        # prints the secret ONCE — copy it now
```

Back on your workstation:

```sh
aimem hub add home https://hub.example.com:8440 "<writer-token>" --pin sha256-...
```

Use the `--pin` value the hub installer printed: it verifies the hub's
self-signed certificate. Once you install a real certificate, the pin
stops matching: run the same `hub add` with `--system-roots` in place of
`--pin`, which drops the pin and keeps the rest of the entry.
Never use the hub's env token here; it is the hub's admin credential.

Checkpoints now push to the hub as they happen; if it is unreachable they spool locally
and flush on the next contact, so capture never depends on the network.
Periodic sync — the leg that pulls curated knowledge back down — rides
the same token (`bash ./install.sh enable-sync` on Linux; Windows gets a
scheduled task automatically).

Repeat — one minted token per machine — for every machine you code
from. That is the whole of "shared memory across machines".

### 2.4 Open the console

```
https://hub.example.com:8440/admin
```

Paste the env token from 2.1. This is where you will actually operate the hub: browse
the knowledge base, organize chapters, review stale facts, edit shared
documents, configure models, watch spend, read logs.

---

## Stage 3 — curation

So far the hub stores turns. Curation is what turns them into knowledge:
a scheduled pass that reads new journal events and extracts durable,
typed, sourced facts.

### 3.1 Give it a model

In the console, open **AI Setup** and add a provider — any
OpenAI-compatible endpoint: the vendor API, a LiteLLM or vLLM proxy,
Ollama on the same network. Each model gets a **test** button; use it, so
a bad key fails in front of you rather than silently at 03:15.

Pick two models:

- a **curate** model — cheap and fast is right; this is extraction, not
  reasoning;
- an **embed** model — enables semantic recall.

Equivalently, in `~/.config/aimem/env` on the hub:

```sh
AIMEM_OPENAI_API_KEY=<key>
AIMEM_OPENAI_BASE_URL=https://api.openai.com/v1   # or your proxy
AIMEM_CURATE_MODEL=gpt-4o-mini
AIMEM_EMBED_MODEL=text-embedding-3-large
```

### 3.2 Cap the spend first

Before the first scheduled run, not after:

```sh
aimem budget --daily 500k --monthly 5M
```

Enforcement is pre-spend — the usage so far plus a worst-case projection
of the next run must fit under the cap — so a cap cannot be overrun.

### 3.3 Run it once by hand

```sh
aimem curate --backend openai --all
aimem embed --all
```

Then look at what it found, in the console's **Knowledge Base** tab or,
with the ID `aimem project-id .` prints in the project:

```sh
aimem recall -p <project> -q "how do we handle migrations"
```

Recall is hybrid: full-text and semantic, fused. A query that shares no
keywords with the fact still finds it.

From here the hourly timer keeps it current on its own. Projects with no
new events cost nothing.

### 3.4 Let the agent use it

MCP was registered when you wired the project, so the agent can query
memory as a tool and record facts itself. Ask it to remember something
durable:

> Remember that migrations run through `make db-migrate`, never psql
> directly.

Then start a fresh session and ask how migrations run.

---

## Where to go next

| You want to | Read |
|---|---|
| Know which storage kind fits what (facts vs docs vs wiki), or wire a big project end to end | [STORAGE-GUIDE.md](STORAGE-GUIDE.md) |
| Keep a living API reference many agents edit at once | structured collections in [USER-MANUAL.md](USER-MANUAL.md) |
| Share facts between related projects | knowledge groups in [USER-MANUAL.md](USER-MANUAL.md) |
| Keep work and personal memory on separate servers | multiple hubs in [INSTALL-CLIENT.md](INSTALL-CLIENT.md#several-hubs-on-one-machine) |
| Organize a growing knowledge base | chapters in [USER-MANUAL.md](USER-MANUAL.md) |
| Tune timers, budgets, or troubleshoot | [ADMIN-MANUAL.md](ADMIN-MANUAL.md) |
| Teach your agent to use memory well | [AI-MANUAL.md](AI-MANUAL.md) |
| Understand why it is built this way | [DESIGN.md](DESIGN.md) |

## If something is not working

**No sessions in `aimem sessions`.** The session was already running when
you installed — restart it. Then check `aimem health` and that
`~/.claude/settings.json` has the `Stop` hook.

**Every turn journaled twice.** Checkpoint hooks are registered at both
user and project level. Remove the project-level ones: they belong at
user level only.

**Hub push failing.** Check the token, and that the client trusts the
certificate (`--pin`, as the hub installer printed it, for a self-signed
one). Nothing is lost while
it fails — events spool and flush later.

**Curation produces nothing.** Check the model's test button in the
console, that the hub can reach the endpoint, and the Log tab — a
zero-yield run reports why.
