# Quickstart

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
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
```

```powershell
# Windows
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex"
```

**Restart your Claude Code or OpenCode session** — hooks and plugins load
at startup, so a session already running will not journal.

Now work normally for a few turns, then:

```sh
aimem timeline
```

Every turn is there: request, reply, tools, files touched. Nothing you
did required an LLM call or a network round trip.

### What just became true

- A crash or `/compact` no longer loses the thread. `docs/SESSION-STATE.md`
  is re-injected into the agent's context at session start, and the full
  journal is a query away.
- Your agent can search its own history:
  ```sh
  aimem search "retry logic"
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
bash <(curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/install-hub.sh)
```

Substitute your own hostname. `AIMEM_DOMAIN` generates a self-signed
certificate and serves TLS immediately; `AIMEM_HUB_NAME` labels the
console, which matters the moment you run a second hub.

The installer prints a **bearer token** at the end (once — re-runs
never re-show it). Keep it: it is the hub's **admin** credential — the
one you paste into the console, and the one that can change
configuration. Workstations get their own tokens in the next step.

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
aimem hub add home https://hub.example.com:8440 "<writer-token>" --insecure
```

Drop `--insecure` once you install a real certificate. Checkpoints now
push to the hub as they happen; if it is unreachable they spool locally
and flush on the next contact, so capture never depends on the network.
Periodic sync — the leg that pulls curated knowledge back down — rides
the same token (`./install.sh enable-sync` on Linux; Windows gets a
scheduled task automatically).

Repeat — one minted token per machine — for every machine you code
from. That is the whole of "shared memory across machines". (The admin
token from 2.1 also works everywhere, but then every machine holds the
key to the hub's configuration, and revoking one machine means
re-keying all of them.)

### 2.4 Open the console

```
https://hub.example.com:8440/admin
```

Paste the token. This is where you will actually operate the hub: browse
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

Then look at what it found, in the console's **Knowledge Base** tab or:

```sh
aimem recall "how do we handle migrations"
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

**No turns in `aimem timeline`.** The session was already running when
you installed — restart it. Then check `aimem health` and that
`~/.claude/settings.json` has the `Stop` hook.

**Every turn journaled twice.** Checkpoint hooks are registered at both
user and project level. Remove the project-level ones: they belong at
user level only.

**Hub push failing.** Check the token, and that the client trusts the
certificate (`--insecure` for a self-signed one). Nothing is lost while
it fails — events spool and flush later.

**Curation produces nothing.** Check the model's test button in the
console, that the hub can reach the endpoint, and the Log tab — a
zero-yield run reports why.
