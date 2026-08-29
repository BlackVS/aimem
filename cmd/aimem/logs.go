package main

// `aimem logs` — local visibility for the errors that used to hide:
// client-side warnings land in <state-root>/adapter.log (spooled
// checkpoints, orphaned hub bindings, doc conflicts — the submit
// process's stderr is discarded by OpenCode's detached spawn and
// buried by Windows hook plumbing), and the service keeps an in-memory
// ring (its stderr reaches journald on Linux but nothing on Windows).
// This command shows both, newest last.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 40, "lines per section")
	q := fs.String("q", "", "only lines containing this substring")
	fs.Parse(args)

	fmt.Println("── client warnings (adapter.log: spooling, orphaned bindings, doc conflicts) ──")
	p := filepath.Join(stateRoot(), "adapter.log")
	if raw, err := os.ReadFile(p); err != nil {
		fmt.Println("(none recorded)")
	} else {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if *q != "" {
			var kept []string
			for _, l := range lines {
				if strings.Contains(l, *q) {
					kept = append(kept, l)
				}
			}
			lines = kept
		}
		if len(lines) > *n {
			lines = lines[len(lines)-*n:]
		}
		for _, l := range lines {
			fmt.Println(l)
		}
	}

	fmt.Println("\n── service log ring (lost on restart; journald keeps the durable copy on Linux) ──")
	resp, err := client().Get(fmt.Sprintf("http://aimem/v1/logs?limit=%d", *n))
	if err != nil {
		fmt.Printf("(service not reachable: %v)\n", err)
		return nil
	}
	defer resp.Body.Close()
	var res struct {
		Entries []struct {
			TS    string `json:"ts"`
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Attrs string `json:"attrs"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	shown := 0
	for _, e := range res.Entries {
		line := fmt.Sprintf("%s %-5s %s %s", e.TS, e.Level, e.Msg, e.Attrs)
		if *q != "" && !strings.Contains(line, *q) {
			continue
		}
		fmt.Println(strings.TrimRight(line, " "))
		shown++
	}
	if shown == 0 {
		fmt.Println("(no matching entries)")
	}
	return nil
}
