# First-pilot hub runbook

This runbook prepares an aimem hub for the first AIForge pilot: one aicrew team with one coordinator and one worker, on one project. It lists every operator command in order, what each one prints, and how to verify the result.

The commands and outputs were captured from a disposable local hub. IDs, dates and host names are placeholders:
- `hub.example.test:8443` is the hub's TLS listener;
- `aicrew.example.test:9443` is aicrew's service;
- `aicrew-example` is aicrew's service ID;
- `team-pilot` is aicrew's team ID;
- `pilot` is the project.

What each step means is in [CHANGELOG `[Unreleased]`](../CHANGELOG.md), "Configuration for an aicrew team", and in the wire contracts (`DESIGN-AIFORGE-*.md`).

## Before you start

- **Versions.** The hub runs a release that includes the AIForge prerequisites. Check with `curl -s https://hub.example.test:8443/v1/status`: `version` must name that release. The coordinator's and worker's machines run the same aimem release.
- **Lockstep with aicrew.** aicrew must send `evidence_digest` on `accepted_for_finalization` (its counterpart of aimem's C5-w3). Without it, every coordinated finalize is refused. Deploy both together, or aicrew first.
- **An admin bearer in a private file.** The `aimem identity` commands read the hub-admin bearer from `--admin-token-file`. That file must be one line and readable only by you:
  - on Windows: `icacls admin.token /inheritance:r /grant:r "%USERNAME%:F"`;
  - on Linux or macOS: `chmod 600 admin.token`.

  The CLI refuses a wider file and prints the exact command to fix it. Write it with a plain LF line ending: a stray carriage return makes the hub answer `401`.
- **Two kinds of command:**
  - `aimem identity ...` runs from any machine, over the hub's TLS listener. Every such command takes the same hub flags, written `$HUB` below:

    ```sh
    HUB="--hub https://hub.example.test:8443 --admin-token-file admin.token"
    # add --hub-ca-file PATH or --hub-pin sha256-BASE64 when the hub's
    # certificate is not in the system roots
    ```
  - **For each member's agent home,** the operator provisions the member's own aimem installation once, as described in step 6. The member sets nothing.
  - `aimem tasks`, `aimem process` and `aimem access` run on the hub host itself, against the local service.
- **One OS account, one agent home per member.** Both members run under one OS account. Each member has an aicrew agent home that holds the member's own aimem installation at `<home>/aimem`: its own `hub.json`, token, team sessions and spool. The human's installation under the account's default state root is not used by either member. aicrew's launcher and the home's own settings name the home's installation for every process started there.

## 1. The hub terminates TLS itself (P5)

Identity routes and team mode are served only over TLS that the hub terminates itself: `AIMEM_TLS_CERT` and `AIMEM_TLS_KEY` on the hub service. A TLS-terminating proxy in front of the hub makes every team request fail. Verify through the exact URL the agents and aicrew will use:

```sh
aimem identity peer list $HUB
```

**Expected on a fresh hub:**

```
no identity peer is registered
```

**If TLS is terminated in front of the hub,** the same request reaches it as plain HTTP. A forwarded header claiming https does not change that. The hub refuses:

```
{"code":"tls_required","message":"This route requires TLS terminated by the hub.","retryable":false,"next_action":"Connect to the hub's TLS listener with certificate verification.","correlation_id":"…"}
```

The CLI itself refuses a plain `http://` hub:

```
aimem: --hub must be the hub's TLS listener as https://HOST:PORT (plain http is refused)
```

## 2. The pilot project: tasks on, process selected

On the hub host. `tasks on` creates the project if it does not exist yet: the grant in step 5 needs an existing project, so run this first.

```sh
aimem tasks on -p pilot
```

**Expected:**

```
tasks on for project "pilot"
```

Select the project's process: a coordinated claim needs the process pin. The reference is recorded as given; it is not fetched here.

```sh
aimem process select https://git.example.test/org/process.git 0123456789abcdef0123456789abcdef01234567 process/manifest.json -p pilot
```

**Expected:**

```
{"current":{"repo":"https://git.example.test/org/process.git","commit":"0123456789abcdef0123456789abcdef01234567","manifest":"process/manifest.json","selected_at":"…","selected_by":"local"},"project":"pilot"}
```

Use the real process repository, a full 40-character commit and the manifest path. A selection must be `https`, `ssh` or `git@`.

## 3. Register aicrew as the identity peer

`--endpoint` is aicrew's introspection route. `--peer-trust-dns` trusts the endpoint's certificate by its name and the system roots; `--peer-trust-pin sha256-BASE64` pins it instead.

```sh
aimem identity peer register aicrew-example \
  --endpoint https://aicrew.example.test:9443/v1/crew/introspect \
  --peer-trust-dns $HUB
```

**Expected:**

```
identity peer aicrew-example registered (endpoint trust ca_dns aicrew.example.test); introspection not operational yet; see aimem identity peer check aicrew-example --hub https://hub.example.test:8443 --admin-token-file admin.token
```

```sh
aimem identity peer list $HUB
```

**Expected** (the hub ID is this hub's own; aicrew and the members need it):

```
aicrew-example  enabled  hub 01a0…-…
  introspection endpoint https://aicrew.example.test:9443/v1/crew/introspect (not operational)
  endpoint trust ca_dns aicrew.example.test
```

## 4. The introspection credential and aicrew's two credentials

**The hub's outbound introspection credential.** aicrew issues it. Put it in a private file on the hub host, set `AIMEM_INTROSPECTION_TOKEN_FILE` to that file's path for the hub service, and restart the service. The hub rereads the file on every call, so a later rotation needs no restart.

**Check it end to end.** The hub sends aicrew one introspection with a random handle and expects the inactive answer:

```sh
aimem identity peer check aicrew-example $HUB
```

**Expected:**

```
identity peer aicrew-example introspection works: the peer verified and answered a probe as inactive
```

**Before the file is set,** it names the missing step and exits non-zero:

```
aimem: identity peer aicrew-example introspection check failed: not_configured (the hub has no AIMEM_INTROSPECTION_TOKEN_FILE set)
```

**aicrew's two credentials.** One redeems identity proofs; the other reads the reservation scope. Each bearer is written once, to a new file only you can read; it is never printed.

```sh
aimem identity cred issue aicrew-example --expires 90d --secret-file redeem.secret $HUB
aimem identity cred issue aicrew-example --expires 90d --secret-file read.secret --operation reservation.read $HUB
```

**Expected:**

```
issued credential 01a0…-… (identity.redeem) for aicrew-example, expiring …
the bearer was written once to redeem.secret (readable only by you); move it into aicrew's protected storage, then delete the file
issued credential 01a0…-… (reservation.read) for aicrew-example, expiring …
the bearer was written once to read.secret (readable only by you); move it into aicrew's protected storage, then delete the file
```

Deliver both files to aicrew's protected storage, then delete them. Verify:

```sh
aimem identity cred list aicrew-example $HUB
```

**Expected:**

```
credentials of aicrew-example:
  01a0…-…  active   created …  expires …  identity.redeem
  01a0…-…  active   created …  expires …  reservation.read
```

**Rotation:** `aimem identity cred rotate ... --operation ...` issues the second credential of that operation. After aicrew has switched to it, revoke the old one with `aimem identity cred revoke aicrew-example CREDENTIAL_ID $HUB`.

## 5. The team's access profile and its grant

The profile links aicrew's team to this hub. A team session reads only the projects its profile is granted, checked live on every request. It never uses a member's personal grants.

```sh
aimem identity team create aicrew-example team-pilot $HUB
aimem identity team grant aicrew-example team-pilot pilot $HUB
```

**Expected:**

```
team profile team-pilot created for aicrew-example (profile 01a0…-…); it has no project grants yet
project pilot (instance 01a0…-…) granted to team team-pilot of aicrew-example
```

A grant for a project that does not exist yet is refused with `hub answered 404: unknown project`: run step 2 first. Verify:

```sh
aimem identity team grants aicrew-example team-pilot $HUB
```

**Expected:**

```
team-pilot  enabled  profile 01a0…-…
  grant pilot  instance 01a0…-…
```

**Revoking:**
- `aimem identity team revoke aicrew-example team-pilot pilot $HUB` removes the grant.
- `aimem identity team disable ...` stops the whole profile.

Both take effect on the next team request.

## 6. Member users and their tokens

On the hub host. Each member (the coordinator and the worker) is an aimem user with a user-scoped token. A project-scoped or read-only token cannot enter team mode.

```sh
aimem access user-add pilot-coordinator
aimem access user-add pilot-worker
```

**Expected (one per user):**

```
{
  "id": "01a0…-…",
  "name": "pilot-coordinator",
  "disabled": false
}
```

Issue each user's token straight into a private file: `token-issue-user` prints the secret once, on standard output. Use it only to provision that member's home (below), then delete it.

```sh
umask 077
aimem access token-issue-user USER_ID pilot-coordinator 2026-12-28T00:00:00Z > coordinator.token.json
```

The file holds `{"secret": "…", "token": {"id": …, "user_id": …, "scope": "user", …}}`. Check that `scope` is `user`. Extract the secret into its own private file, `coordinator.token`, holding that one line.

**Provisioning each member's home.** On the members' machine, once per member, the operator writes the member's hub entry and token into the installation inside that member's agent home. The two variables name that installation for these commands only. The token is read from standard input, never from a command line. It is the member's own user token, used both as the hub entry's token and as the individual credential: a member home runs no aimem service and pushes no checkpoints, so it never needs a hub writer token. Put the hub's CA file inside the home (`<home>/creds/`), so nothing in the home's `hub.json` points outside it. A CA file that cannot be read is refused, naming its path.

Linux or macOS, for the coordinator's home `/srv/agents/coordinator`:

```sh
export AIMEM_STATE_DIR=/srv/agents/coordinator/aimem
export AIMEM_SOCKET=/srv/agents/coordinator/aimem/aimem.sock
mkdir -p -m 700 "$AIMEM_STATE_DIR"
aimem hub add pilot-hub https://hub.example.test:8443 --token-file - \
  --ca-file /srv/agents/coordinator/creds/hub-ca.pem < coordinator.token
aimem hub task-token pilot-hub --token-file - < coordinator.token
aimem hub credential pilot-hub
unset AIMEM_STATE_DIR AIMEM_SOCKET
```

Windows PowerShell, for the coordinator's home `C:\agents\coordinator`:

```powershell
$env:AIMEM_STATE_DIR = 'C:\agents\coordinator\aimem'
$env:AIMEM_SOCKET = 'C:\agents\coordinator\aimem\aimem.sock'
New-Item -ItemType Directory -Force $env:AIMEM_STATE_DIR | Out-Null
Get-Content coordinator.token -TotalCount 1 | aimem hub add pilot-hub https://hub.example.test:8443 --token-file - --ca-file C:\agents\coordinator\creds\hub-ca.pem
Get-Content coordinator.token -TotalCount 1 | aimem hub task-token pilot-hub --token-file -
aimem hub credential pilot-hub
Remove-Item Env:AIMEM_STATE_DIR, Env:AIMEM_SOCKET
```

Repeat with the worker's home and `worker.token`. Instead of `--ca-file`, `--pin sha256-BASE64` pins the hub's certificate by its public key.

**Expected:**

```
hub "pilot-hub" configured (default: pilot-hub)
task credential stored for hub "pilot-hub"; MCP task tools use it
hub pilot-hub: individual credential set, active, scope user (user 01a0…-…, token 01a0…-…)
```

Then delete the token files. Rules for this layout:
- **Each process names its installation.** `AIMEM_STATE_DIR` names the installation and `AIMEM_SOCKET` its socket. aicrew's launcher and the home's settings set both for every process started in the home, so the member sets nothing. With an aimem release that includes the socket rule (an explicit `AIMEM_STATE_DIR` keeps the socket inside it), the explicit `AIMEM_SOCKET` is a second safeguard; with v0.7.4 it is required on Linux.
- **The user-wide env file must not name an installation.** `~/.config/aimem/env` belongs to the whole OS account, and aimem folds its `AIMEM_*` values into every process that lacks its own. It must not set `AIMEM_STATE_DIR` or `AIMEM_SOCKET`: a process started without its own value would silently use the installation the file names.
- **A member home runs no aimem service.** There is no `aimem serve` for a home, so the home's socket is never bound. Team mode needs none, and memory tools in a standalone session started in a home are unavailable.
- **One account isolates identities, not files.** Both homes belong to one OS account, so each member's processes can read the other member's home, including its `aimem/hub.json` with the token, and reach its step socket. The hub still tells the two members apart. A separate OS account per member is the only host isolation, and needs no change here.

**Verify the token over the hub's TLS** without putting the secret on a command line. Keep it in a private header file:

```sh
# coordinator.auth holds one line: Authorization: Bearer <the secret>
curl -s -H @coordinator.auth "https://hub.example.test:8443/v1/access/identity?project=pilot"
```

**Expected:**

```
{"name":"pilot-coordinator","project":"pilot","role":"user","scope":"user","task_read":"all-projects","task_write":false,"tasks_enabled":true,"token_id":"…","user_id":"…"}
```

`task_write: false` is expected. The member has no personal grant on the project, and in team mode only the profile's grant counts. Give a personal grant (`aimem access grant add pilot user USER_ID`) only if the member should also work in the project outside the team.

## 7. Linking each member through an identity proof

**The member does this, not the operator.** aicrew's client drives it at onboarding and at every session start:
1. aicrew issues a challenge.
2. The member's aimem asks the hub for a single-use proof receipt (`aimem identity proof --peer aicrew-example --hub-id HUB_ID --challenge ID`). It writes the receipt only into a pipe to aicrew's client, never to a terminal or a file.
3. aicrew redeems it with its `identity.redeem` credential.

The operator never sees a receipt. `HUB_ID` is the hub ID from `aimem identity peer list`.

**Verify from the hub side,** after a member has started a team session. The hub's access audit records `team.verified` for each verified team request. A refusal is recorded as `team.refused.<code>` with a correlation ID matching the one the member saw. On the hub host, `aimem access list` prints them under `recent_audit`, newest first, next to the users, grants and tokens. The setup above is there too: `identity_peer.register`, `identity_peer.credential.issue.<operation>`, `team_profile.create`, `team_grant.true`, `user.create` and `token.issue`.

## Checklist

| Step | Verified by |
| --- | --- |
| TLS terminated by the hub | `aimem identity peer list $HUB` answers; no `tls_required` |
| Pilot project | `aimem tasks on -p pilot`; `aimem process select …` echoes the selection |
| aicrew peer | `aimem identity peer list $HUB` shows it `enabled` |
| Introspection credential | `aimem identity peer check aicrew-example $HUB` says it works |
| aicrew's two credentials | `aimem identity cred list aicrew-example $HUB` shows `identity.redeem` and `reservation.read` active |
| Team profile and grant | `aimem identity team grants aicrew-example team-pilot $HUB` lists `pilot` |
| Member tokens | `GET /v1/access/identity` answers `scope: user` for each member |
| Member homes | with each home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, `aimem hub credential pilot-hub` answers `set` and `active`; `~/.config/aimem/env` sets neither variable |
| Members linked | a member's session start is audited as `team.verified` |

Secrets never belong in a command line, a shell history, a log, a chat or a task comment. Every secret in this runbook travels in a file only its owner can read.
