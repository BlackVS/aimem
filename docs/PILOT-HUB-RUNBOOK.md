# First-pilot hub runbook

This runbook prepares an aimem hub for the first AIForge pilot: one aicrew team with one coordinator and one worker, on one project. It lists every operator command in order, what each one prints, and how to verify the result.

The commands and outputs were captured from a disposable local hub. IDs, dates and host names are placeholders:
- `hub.example.test:8443` is the hub's TLS listener;
- `aicrew.example.test:9443` is aicrew's service;
- `aicrew-example` is aicrew's service ID;
- `0190…-…` is aicrew's team UUID, and `pilot` is the team's name, which aicrewd registers;
- `pilot` is also the project.

What each step means is in [CHANGELOG `[Unreleased]`](../CHANGELOG.md), "Configuration for an aicrew team", and in the wire contracts (`DESIGN-AIFORGE-*.md`).

## Before you start

- **Versions.** The hub runs a release that includes the AIForge prerequisites. Check with `curl -s https://hub.example.test:8443/v1/status`: `version` must name that release. The coordinator's and worker's machines run the same aimem release.
- **Lockstep with aicrew.** aicrew must send `evidence_digest` on `accepted_for_finalization` (its counterpart of aimem's C5-w3). Without it, every coordinated finalize is refused. Deploy both together, or aicrew first.
- **An admin bearer in a private file.** The `aimem identity` commands read the hub-admin bearer from `--admin-token-file`. That file must be one line and readable only by you:
  - on Windows: `icacls admin.token /inheritance:r /grant:r "%USERNAME%:F"`;
  - on Linux or macOS: `chmod 600 admin.token`.

  The CLI refuses a wider file and prints the exact command to fix it. It trims the line ending, so a CRLF-written file works.
- **No carriage return in the hub's own token.** When the admin bearer is the hub's environment token, the hub compares it with its `AIMEM_HTTP_TOKEN` exactly. If that value keeps a trailing carriage return, the bearer fails with `hub answered 401: Unauthorized`. Named admin tokens (`aimem token add`) are not affected. A carriage return gets into the value, for example, when a service script sets it with `$(cat FILE)` from a CRLF-written file: command substitution drops the newline but keeps the carriage return. aimem's own reader of `~/.config/aimem/env` trims line endings, but a value the service manager or a wrapper script has already set wins over the file. Write any file the hub service's environment comes from with LF line endings; `grep -c $'\r' FILE` must print `0`.
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

## 2. The pilot project: tasks on, process selected, repository set

On the hub host. `tasks on` creates the project if it does not exist yet: the grant in step 5 needs an existing project, so run this first.

```sh
aimem tasks on --project pilot
```

**Expected:**

```
tasks on for project "pilot"
```

Select the project's process: a coordinated claim needs the process pin. The reference is recorded as given; it is not fetched here.

```sh
aimem process select https://git.example.test/org/process.git 0123456789abcdef0123456789abcdef01234567 process/manifest.json --project pilot
```

**Expected:**

```
{"current":{"repo":"https://git.example.test/org/process.git","commit":"0123456789abcdef0123456789abcdef01234567","manifest":"process/manifest.json","selected_at":"…","selected_by":"local"},"project":"pilot"}
```

Use the real process repository, a full 40-character commit and the manifest path. A selection must be `https`, `ssh` or `git@`.

Set the project's repository: the one repository the pilot's work happens in. `--kind` is the forge's API dialect (`github`, `gitea` or `gitlab`), and the host of `--url`, with its port when the URL names one, names the credential a member needs. `--access` is `write` (the default) or `read`. The hub stores the URL as given, never fetches it and stores no default branch. A URL carrying a user name on `https`, or any password, is refused.

```sh
aimem project repo set --project pilot --kind gitea --url https://git.example.test/org/pilot.git
```

**Expected:**

```
project.repository.set for project "pilot"
  previous: (none)
  now:      gitea https://git.example.test/org/pilot.git (host git.example.test, access write)
```

`aimem project show --project pilot` prints the repository, the process pin and the project's grants with names beside IDs. Run it again after step 5 to see the team profile's grant. `aimem project repo clear --project pilot` removes the repository; every set and clear is kept in the project's repository history with the values before and after.

## 3. Provision aicrew as the identity peer

One command registers aicrew as the hub's identity peer and writes aicrew's four credentials into a directory, one file each, readable only by you. Each credential permits exactly one operation:

| File | Operation | What aicrew uses it for |
|---|---|---|
| `aimem-redeem.token` | `identity.redeem` | redeem identity proofs |
| `aimem-read.token` | `reservation.read` | read the reservation scope |
| `aimem-team-register.token` | `team.register` | create and name its own teams' profiles (step 5) |
| `aimem-team-read.token` | `team.read` | read its own teams' grants, each granted project's repository and its process pin |

`--endpoint` is aicrew's introspection route. `--peer-trust-dns` trusts the endpoint's certificate by its name and the system roots; `--peer-trust-pin sha256-BASE64` pins it instead. The credentials expire after 90 days unless `--expires` says otherwise.

```sh
aimem identity peer provision aicrew-example \
  --endpoint https://aicrew.example.test:9443/v1/crew/introspect \
  --peer-trust-dns --output-dir aicrew-creds $HUB
```

**Expected** (the hub ID is this hub's own; aicrew and the members need it):

```
identity peer aicrew-example registered (endpoint trust ca_dns aicrew.example.test)
issued identity.redeem credential 01a0…-… into aicrew-creds/aimem-redeem.token, expiring …
issued reservation.read credential 01a0…-… into aicrew-creds/aimem-read.token, expiring …
issued team.register credential 01a0…-… into aicrew-creds/aimem-team-register.token, expiring …
issued team.read credential 01a0…-… into aicrew-creds/aimem-team-read.token, expiring …
hub ID 01a0…-…
```

aicrew reads the directory with `aicrew hub add --cred-dir aicrew-creds`. The bearers are never printed.

**Running it again** is safe. It keeps every file that holds a credential, issues only for a file that is missing or empty, and says so; a provisioning that was cut short is finished this way. A file that holds a credential is never overwritten: for a peer that is not registered yet, the command refuses before it changes anything.

**Replacing the peer.** The hub allows one enabled peer. To move aicrew to another service ID, name the old peer with `--replace`: it is disabled in the same step (and enabled again if the new registration fails). Then retire the old peer, so the new one can take its teams; `team.register` refuses a team UUID that another peer's profile holds, even a disabled one:

```sh
aimem identity peer provision aicrew-renamed \
  --endpoint https://aicrew.example.test:9443/v1/crew/introspect \
  --peer-trust-dns --output-dir aicrew-creds-renamed --replace aicrew-example $HUB
aimem identity peer retire aicrew-example $HUB
```

**Expected** from the retirement:

```
identity peer aicrew-example retired; removed credentials=5 team_profiles=1 team_grants=1 receipts=2 redemptions=2; its name and teams can be registered again
```

Retirement removes the peer with its credentials, team profiles and their grants, proof receipts and redemptions, and keeps the audit history. Grant the project again to the new peer's profile once aicrew has registered the team, and expect each member to prove again on the next join.

## 4. The introspection credential

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

**aicrew's credentials** are listed with:

```sh
aimem identity cred list aicrew-example $HUB
```

**Expected:**

```
credentials of aicrew-example:
  01a0…-…  active   created …  expires …  identity.redeem
  01a0…-…  active   created …  expires …  reservation.read
  01a0…-…  active   created …  expires …  team.register
  01a0…-…  active   created …  expires …  team.read
```

**Rotation:** `aimem identity cred rotate ... --operation ...` issues the second credential of that operation. After aicrew has switched to it, revoke the old one with `aimem identity cred revoke --peer aicrew-example --credential CREDENTIAL_ID $HUB`. `--output -` writes a bearer to standard output for a pipe into aicrew's own command instead of a file; it is refused when standard output is a terminal.

## 5. The team's access profile and its grant

The profile links aicrew's team to this hub. A team session reads only the projects its profile is granted, checked live on every request. It never uses a member's personal grants.

aicrewd creates the profile itself: `aicrew team create` registers the team's UUID and name with `team.register`, and `aicrew team rename` re-registers it. The name is unique per peer. The operator then grants the project by that name; the grant binds the team's UUID, which the command prints, so a later rename moves no grant.

```sh
aimem identity team grant --peer aicrew-example --team-name pilot --project pilot $HUB
```

**Expected:**

```
project pilot (instance 01a0…-…) granted to team pilot (0190…-…) of aicrew-example
```

An unknown name is refused with the names the peer has registered. Until aicrewd registers its teams, `aimem identity team create --peer aicrew-example --team-id TEAM_UUID $HUB` still creates a profile by UUID for this release (with a notice), and `--team-id TEAM_UUID` names it in the commands below.

A grant for a project that does not exist yet is refused with `hub answered 404: unknown project`: run step 2 first. Verify:

```sh
aimem identity team grants --peer aicrew-example --team-name pilot $HUB
```

**Expected:**

```
pilot  0190…-…  enabled  profile 01a0…-…
  grant pilot  instance 01a0…-…
```

**Revoking:**
- `aimem identity team revoke --peer aicrew-example --team-name pilot --project pilot $HUB` removes the grant.
- `aimem identity team disable ...` stops the whole profile.

Both take effect on the next team request.

## 6. Member users and their tokens

On the hub host. Each member (the coordinator and the worker) is an aimem user with a user-scoped token. A project-scoped or read-only token cannot enter team mode.

```sh
aimem access user-add --user-name pilot-coordinator
aimem access user-add --user-name pilot-worker
```

**Expected (one per user):**

```
{
  "id": "01a0…-…",
  "name": "pilot-coordinator",
  "disabled": false
}
```

Issue each user's token straight into a private file: `token-issue-user` writes the secret once to `--output`, a new file only you can read, and prints the token's record without it. Use the file only to provision that member's home (below), then delete it.

```sh
aimem access token-issue-user --user-name pilot-coordinator --label pilot-coordinator --expires 2026-12-28T00:00:00Z --output coordinator.token
```

**Expected:**

```
{
  "token": {
    "id": "01a0…-…",
    "user_id": "01a0…-…",
    "label": "pilot-coordinator",
    "scope": "user",
    "expires_at": "2026-12-28T00:00:00Z",
    "revoked": false
  }
}
the secret was written once to coordinator.token (readable only by you)
```

Check that `scope` is `user`. `coordinator.token` holds the secret on one line.

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

The last line is the check the member's identity proof (step 7) depends on, and the only one that tests the home itself. Run `aimem hub credential pilot-hub` again in each home, with that home's two variables set, whenever a home is rebuilt or a token replaced. Anything but `set, active, scope user` means the proof cannot succeed:
- `hub "pilot-hub" is not configured on this machine`: the home was never provisioned, or the variables name another installation. The proof itself would fail with `no hub is configured on this machine (aimem hub add)`.
- `individual credential none`: `aimem hub task-token` was not run in this home.
- `the stored credential is not an individual credential`: the home holds something other than the member's user token.
- `refused by the hub`: the hub does not accept the token (revoked, expired or unknown). Issue a new one and provision the home again.
- `the hub is unreachable`: the URL, the CA file or the pin in the home's `hub.json` does not match the hub's TLS listener; the detail in parentheses names the failure.
- `active, scope project` or `scope read-only`: the token is not user-scoped and cannot enter team mode.

Then delete the token files. Rules for this layout:
- **Each process names its installation.** `AIMEM_STATE_DIR` names the installation and `AIMEM_SOCKET` its socket. aicrew's launcher and the home's settings set both for every process started in the home, so the member sets nothing. With an aimem release that includes the socket rule (an explicit `AIMEM_STATE_DIR` keeps the socket inside it), the explicit `AIMEM_SOCKET` is a second safeguard; with v0.7.4 it is required on Linux.
- **The user-wide env file must not name an installation.** `~/.config/aimem/env` belongs to the whole OS account, and aimem folds its `AIMEM_*` values into every process that lacks its own. It must not set `AIMEM_STATE_DIR` or `AIMEM_SOCKET`: a process started without its own value would silently use the installation the file names.
- **A member home runs no aimem service.** There is no `aimem serve` for a home, so the home's socket is never bound. Team mode needs none, and memory tools in a standalone session started in a home are unavailable.
- **One account isolates identities, not files.** Both homes belong to one OS account, so each member's processes can read the other member's home, including its `aimem/hub.json` with the token, and reach its step socket. The hub still tells the two members apart. A separate OS account per member is the only host isolation, and needs no change here.

**Verify the token over the hub's TLS** without putting the secret on a command line. This checks the token only, not the member's home: it passes before the home is provisioned. Keep it in a private header file:

```sh
# coordinator.auth holds one line: Authorization: Bearer <the secret>
curl -s -H @coordinator.auth "https://hub.example.test:8443/v1/access/identity?project=pilot"
```

**Expected:**

```
{"name":"pilot-coordinator","project":"pilot","role":"user","scope":"user","task_read":"all-projects","task_write":false,"tasks_enabled":true,"token_id":"…","user_id":"…"}
```

`task_write: false` is expected. The member has no personal grant on the project, and in team mode only the profile's grant counts. Give a personal grant (`aimem access grant add --project pilot --user-name NAME`) only if the member should also work in the project outside the team.

## 7. Linking each member through an identity proof

**The member does this, not the operator.** aicrew's client drives it at onboarding and at every session start:
1. aicrew issues a challenge.
2. The member's aimem asks the hub for a single-use proof receipt (`aimem identity proof --peer aicrew-example --hub-id HUB_ID --challenge ID`). It writes the receipt only into a pipe to aicrew's client, never to a terminal or a file.
3. aicrew redeems it with its `identity.redeem` credential.

The operator never sees a receipt. `HUB_ID` is the hub ID from `aimem identity peer list`.

**Prerequisite: the member's home is provisioned (step 6).** The proof runs in the member's own installation. It presents the individual credential that `aimem hub task-token` stored in the home, and it reaches the hub through the URL and the CA file or pin in the home's `hub.json`. Delivering the token file or the header file configures neither. Before the member's first session, `aimem hub credential pilot-hub`, run in that home, must answer `set, active, scope user`.

**Verify from the hub side,** after a member has started a team session. The hub's access audit records `team.verified` for each verified team request. A refusal is recorded as `team.refused.<code>` with a correlation ID matching the one the member saw. On the hub host, `aimem access list` prints them under `recent_audit`, newest first, next to the users, grants and tokens. The setup above is there too: `identity_peer.register`, `identity_peer.credential.issue.<operation>`, `team.register.created` (by `peer:aicrew-example`), `team_grant.true`, `user.create` and `token.issue`. Each `team.read` of aicrewd is recorded as `team.read`.

## Checklist

| Step | Verified by |
| --- | --- |
| TLS terminated by the hub | `aimem identity peer list $HUB` answers; no `tls_required` |
| Pilot project | `aimem tasks on --project pilot`; `aimem process select …` echoes the selection; `aimem project show --project pilot` prints the repository and the pin |
| aicrew peer | `aimem identity peer list $HUB` shows it `enabled` |
| Introspection credential | `aimem identity peer check aicrew-example $HUB` says it works |
| aicrew's four credentials | `aimem identity cred list aicrew-example $HUB` shows `identity.redeem`, `reservation.read`, `team.register` and `team.read` active |
| Team profile and grant | `aimem identity team grants --peer aicrew-example --team-name pilot $HUB` lists `pilot` |
| Member tokens | `GET /v1/access/identity` answers `scope: user` for each member |
| Member homes, before the first proof | with each home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, `aimem hub credential pilot-hub` answers `set, active, scope user`; `~/.config/aimem/env` sets neither variable |
| Hub token | `grep -c $'\r'` prints `0` for every file the hub service's environment comes from |
| Members linked | a member's session start is audited as `team.verified` |

Secrets never belong in a command line, a shell history, a log, a chat or a task comment. Every secret in this runbook travels in a file only its owner can read.
