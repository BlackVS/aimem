# Installing an aimem hub

A hub is the same `aimem` binary run as a server. It is where journals
from several machines merge, where curation runs, and where the admin
console lives. It is **optional**: a workstation alone journals, recalls
and survives compaction perfectly well. Add a hub when you want more than
one machine to share one memory.

Target: a fresh Debian or Ubuntu host — an LXC container, a VM, or a small
always-on box. One vCPU and 1 GB of RAM is enough for a personal
deployment.

## 1. One command, as root

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/install-hub.sh | bash
```

It creates the service user, installs the latest release binary, writes
`~/.config/aimem/env`, installs the serve unit and the hourly curation
timer with memory caps, starts everything, and prints a health check plus
the bearer token you will give to clients.

Re-running upgrades the binary and restarts the service. An existing
`env` file is never overwritten, so re-running is safe.

## 2. Options

All of these are environment variables set before the command above.

| Variable | Default | Effect |
|---|---|---|
| `AIMEM_HUB_USER` | `sessiond` | service account, created if missing |
| `AIMEM_HTTP_LISTEN` | `:8440` | listen address |
| `AIMEM_HTTP_TOKEN` | generated | bearer token; printed at the end |
| `AIMEM_HUB_NAME` | hostname | display name in the console and browser tab |
| `AIMEM_DOMAIN` | — | generate a self-signed certificate for this name and serve TLS |
| `AIMEM_TLS_CERT`, `AIMEM_TLS_KEY` | — | explicit certificate paths; override `AIMEM_DOMAIN` |
| `AIMEM_OPENAI_API_KEY` | — | enable curation and embeddings |
| `AIMEM_OPENAI_BASE_URL` | `https://api.openai.com/v1` | any OpenAI-compatible endpoint |
| `AIMEM_CURATE_MODEL` | `gpt-4o-mini` | model that distils facts |
| `AIMEM_EMBED_MODEL` | `text-embedding-3-large` | model for semantic recall |
| `AIMEM_VERSION` | latest | pin a release |
| `AIMEM_REPO` | `BlackVS/aimem` | install from a fork |

Name your hubs if you run more than one — two consoles that both say
"aimem hub" are genuinely confusing:

```sh
AIMEM_HUB_NAME=home AIMEM_DOMAIN=hub.example.com bash install-hub.sh
```

## 3. TLS

Nothing set means plain HTTP, which is only reasonable on a trusted
segment. `AIMEM_DOMAIN=hub.example.com` generates a self-signed
certificate into `~/.config/aimem/tls/` and serves TLS immediately;
clients then connect with `aimem hub add ... --insecure` until you install
a real certificate.

For a real certificate, point `AIMEM_TLS_CERT` and `AIMEM_TLS_KEY` at the
files and restart. Prefer delivering renewed certificates **onto** the hub
from wherever you already do ACME, over giving the hub the credentials to
issue its own: it holds every project's memory, and it should hold as few
other secrets as possible.

## 4. Curation (optional)

Without an API key the hub still stores, merges and serves everything —
recall is BM25 only, and no fact extraction happens. With a key it also:

- runs the curator hourly, distilling journal turns into durable facts;
- backfills embeddings, so recall becomes hybrid BM25 + vector;
- synthesizes a design document per knowledge base.

Any OpenAI-compatible endpoint works: the vendor API, a LiteLLM or vLLM
proxy, or Ollama on the same network. Configuration lives in
`~/.config/aimem/env` and can be edited afterwards from the admin console
(AI Setup tab) rather than by hand.

Curation is metered. Cap it before it spends:

```sh
aimem budget --daily 500k --monthly 5M
```

Enforcement is pre-spend — the usage so far plus a worst-case projection
of the next run must fit under the cap — so a cap cannot be overrun.

## 5. Verify

On the hub:

```sh
systemctl --user -M sessiond@ status aimem      # or: runuser -u sessiond -- systemctl --user status aimem
curl -sk https://127.0.0.1:8440/v1/status       # public: liveness, build, uptime
```

From anywhere:

```sh
curl -s https://hub.example.com:8440/v1/status
curl -s -H "Authorization: Bearer $TOKEN" https://hub.example.com:8440/v1/projects
```

Then open `https://hub.example.com:8440/admin` and paste the token. The
console is the normal way to run a hub: browse the knowledge base, edit
chapters, review stale facts, edit shared documents, configure models,
watch spend, read logs.

Prefer handing each workstation its **own named token** over sharing
the installer's: `aimem token add <machine>` on the hub prints a
writer-role secret once and stores only its hash — see ADMIN-MANUAL
"Named tokens". The API is described at `/v1/openapi.json` (bearer-
gated; the same file ships in the repo).

### What a hub serves

| Path | Auth | Contents |
|---|---|---|
| `/` | none | status card: liveness, build, hostname, uptime |
| `/v1/status` | none | the JSON behind that card |
| `/admin` | none to load | the console shell; it asks for the token in the browser |
| everything else | `Authorization: Bearer <token>` | the whole API |

Those first three are the complete unauthenticated surface. A hub listens
on a routable name, so anything served without the token is served to
whoever can reach the port.

The listener is 8440 rather than 443 because the hub runs as a systemd
`--user` service under an unprivileged account, and ports below 1024 need
`CAP_NET_BIND_SERVICE`. To move it, use a root-owned socket unit, a system
unit with `AmbientCapabilities`, or a reverse proxy.

## 6. Where the state lives

```
~/.config/aimem/env             configuration (0600)
~/.config/aimem/tls/            certificates
~/.local/state/aimem/           journals and memories, one DB per project
~/.local/state/aimem/providers.json   model/provider registry (0600, never synced)
```

Nothing stateful lives inside the binary, so a filesystem backup of the
service user's home captures the entire hub. `providers.json` holds API
tokens and is deliberately host-local: it never syncs between machines.

## 7. Connect clients

Mint each workstation its own writer token here on the hub, then
register it there (see [INSTALL-CLIENT.md](INSTALL-CLIENT.md)):

```sh
aimem token add <machine>                # on the hub; secret shown once
# on the workstation:
aimem hub add <name> https://hub.example.com:8440 "<writer-token>"
```

For day-to-day operation — timers, curation tuning, budgets, multi-hub
partitioning, troubleshooting — see [ADMIN-MANUAL.md](ADMIN-MANUAL.md).
