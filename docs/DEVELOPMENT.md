# Developing aimem

## Build and test as CI does

```sh
CGO_ENABLED=0 go build -o aimem ./cmd/aimem
go test ./...
gofmt -l .                       # must print nothing
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
sh scripts/check-process-stops.sh
```

## Processes and state in tests

A development machine usually runs an aimem installation of its own: a local service (on Windows the scheduled task `aimem-serve`), its state root, its socket, and one MCP server per open client. A test, a script or a hand-run evidence check shares that machine. It must never reach that installation.

**Rule 1: stop only what you started.** Stop a process through the handle you hold: the `exec.Cmd`, a child handle, or the PID your own code started. Never stop by name. That means no `pkill`, `killall`, `taskkill /IM`, `Stop-Process -Name` or `Get-Process … | Stop-Process`. A name matches the installed service as well as your instance.

**Rule 2: isolate the state root and the socket.** Run a test instance under its own `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, both in a temporary directory. The guards catch what slips through:
- In a test binary, the state root (`adapter.StateRoot`) and the local socket (`server.SocketPath`) must lie in the temporary directory, or the test panics (package `internal/isolation`).
- The packages whose tests reach the socket (`cmd/aimem`, `internal/adapter`, `internal/mcp`, `internal/tui`) clear an inherited `AIMEM_SOCKET`, `AIMEM_STATE_DIR` or `XDG_RUNTIME_DIR` that points elsewhere, in their `TestMain`.
- A test that spawns `aimem` passes `AIMEM_SOCKET=` (or its own temporary socket) to the child, instead of inheriting the developer's.

**Rule 3: keep evidence runs by hand to the same rules.** To show a command against a live service for a PR, start a throwaway `aimem serve` with its own `AIMEM_STATE_DIR`, `AIMEM_SOCKET` and `HOME`, keep its PID, and stop that PID. Never stop "every aimem".

**The installers follow the same rule.**
- `install.ps1`'s restart stops only the `aimem serve` processes of the installation it upgrades. It selects them by executable path, the binary or its parked `aimem.exe.old-*` copies, through `Select-ServeProcess`. `internal/installps` tests that against synthetic processes, including another installation's service under the same OS user.
- `install.sh` restarts its own systemd user unit.

**Enforcement.** `scripts/check-process-stops.sh` runs in CI's lint job. It refuses a stop by name in Go tests, `scripts/`, the workflows, and shell, PowerShell and JavaScript files.

**Why.** On 2026-10-04 a throwaway service started for a PR's CLI evidence was stopped with `taskkill /F /IM aimem.exe`, a hand-run command and not repository code. It also ended the developer's installed service and every MCP server that depended on it, and the operator had to restart them. The same audit then found tests that would have reached a real service: with an inherited `AIMEM_SOCKET`, or with the real `XDG_RUNTIME_DIR` on Linux, the local socket of several test packages resolved to the developer's own. The guards above close that path.
