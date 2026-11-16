package main

// aimem group <name> --publish | --adopt | --hub <hub>: the deliberate
// convergence of a group's charter keys with one hub. Ordinary sync is
// fill-only and keeps a differing value on both sides; these commands
// are the explicit way to make one side's value the common one. A
// publish reaches the hub only: every other machine keeps its own value
// until someone runs --adopt there.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aimem/internal/adapter"
	"aimem/internal/curate"
	"aimem/internal/store"
)

// groupHubCmd runs one of the three modes: "publish", "adopt" or
// "compare". Every request goes through hubDo, the client the HTTP sync
// uses, with the hub entry's writer token.
func groupHubCmd(reg *store.Registry, db *store.DB, name, gid, flagHub, mode string) error {
	hubName, h, err := resolveGroupHub(reg, gid, flagHub)
	if err != nil {
		return err
	}
	missing := func() error {
		return fmt.Errorf("hub %q holds no group %q: nothing to read there (`aimem group %s --publish --hub %s` creates it with this machine's config)",
			hubName, gid, name, hubName)
	}
	switch mode {
	case "compare":
		local, onHub := map[string]string{}, map[string]string{}
		var differs []string
		for _, k := range store.GroupConfigKeys {
			v, err := hubMeta(h, gid, k)
			if noSuchGroup(err) {
				return missing()
			}
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			local[k], _ = db.GetMeta(k)
			onHub[k] = v
			if local[k] != v {
				differs = append(differs, k)
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"group": name, "hub": hubName, "local": charterView(local), "on_hub": charterView(onHub),
			"differs": append([]string{}, differs...),
		})
	case "publish":
		fmt.Printf("group %s: publishing this machine's config to hub %q\n", gid, hubName)
		if failed := publishKeys(os.Stdout, db, h, gid); failed > 0 {
			return fmt.Errorf("publish of group %s: %d of %d keys failed (see above)", gid, failed, len(store.GroupConfigKeys))
		}
		return nil
	}
	fmt.Printf("group %s: adopting the config of hub %q\n", gid, hubName)
	failed := 0
	for i, k := range store.GroupConfigKeys {
		local, _ := db.GetMeta(k)
		hv, err := hubMeta(h, gid, k)
		if noSuchGroup(err) {
			return missing()
		}
		if unreachable(err) {
			failed += stopKeys(os.Stdout, k, i, err)
			break
		}
		if err != nil {
			fmt.Printf("  %s: failed to read the hub's value: %v\n", k, err)
			failed++
			continue
		}
		if hv == local {
			fmt.Printf("  %s: unchanged\n", k)
			continue
		}
		if hv == "" {
			fmt.Printf("  %s: kept (empty on the hub)\n", k)
			continue
		}
		if err := checkCharterValue(k, hv); err != nil {
			fmt.Printf("  %s: refused (%v); the local value stays\n", k, err)
			failed++
			continue
		}
		if err := db.SetMeta(k, hv); err != nil {
			fmt.Printf("  %s: failed to save: %v\n", k, err)
			failed++
			continue
		}
		fmt.Printf("  %s: %s here (%s)\n", k, filledOrReplaced(local), describeChange(k, local, hv))
	}
	if failed > 0 {
		return fmt.Errorf("adopt of group %s: %d of %d keys failed (see above)", gid, failed, len(store.GroupConfigKeys))
	}
	return nil
}

// publishKeys writes each non-empty local charter key over the hub's,
// one PUT per key, and reports every key on w. An empty local key is
// never sent, so a publish never clears a hub value. It returns the
// number of keys that failed.
func publishKeys(w io.Writer, db *store.DB, h *adapter.HubConfig, gid string) int {
	failed := 0
	for i, k := range store.GroupConfigKeys {
		local, _ := db.GetMeta(k)
		if local == "" {
			fmt.Fprintf(w, "  %s: not sent (empty on this machine; the hub keeps its value)\n", k)
			continue
		}
		hv, err := hubMeta(h, gid, k)
		switch {
		case noSuchGroup(err):
			hv = "" // the PUT below creates the group on the hub
		case unreachable(err):
			return failed + stopKeys(w, k, i, err)
		case err != nil:
			fmt.Fprintf(w, "  %s: failed to read the hub's value: %v\n", k, err)
			failed++
			continue
		}
		if hv == local {
			fmt.Fprintf(w, "  %s: unchanged\n", k)
			continue
		}
		if err := putHubMeta(h, gid, k, local); unreachable(err) {
			return failed + stopKeys(w, k, i, err)
		} else if err != nil {
			fmt.Fprintf(w, "  %s: hub refused: %v\n", k, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "  %s: %s on the hub (%s)\n", k, filledOrReplaced(hv), describeChange(k, hv, local))
	}
	return failed
}

// publishAfterEdit sends an edit to every hub the group's projects use,
// right away, through publishKeys. Its report goes to stderr, since
// stdout carries the group's JSON. A hub not configured here, or one
// without an API token, is skipped as before.
func publishAfterEdit(reg *store.Registry, db *store.DB, name, gid string) {
	hubs, def := adapter.LoadHubs(stateRoot())
	if hubs == nil {
		return // a local-only install
	}
	for _, hn := range hubNamesForGroup(reg, gid) {
		if hn == "" {
			hn = def
		}
		h, ok := hubs[hn]
		if !ok || h.URL == "" || h.Token == "" {
			continue // as resolveGroupHub refuses it: no API entry for this hub
		}
		fmt.Fprintf(os.Stderr, "aimem: group %s: publishing the edit to hub %q\n", gid, hn)
		if failed := publishKeys(os.Stderr, db, h, gid); failed > 0 {
			fmt.Fprintf(os.Stderr, "aimem: %d key(s) did not reach hub %q; `aimem group %s --publish --hub %s` retries\n", failed, hn, name, hn)
		}
	}
}

// resolveGroupHub picks the hub to converge with: the --hub flag, else
// the one hub the group's bound projects use, else the only hub
// configured. Anything else is ambiguous and asks for --hub.
func resolveGroupHub(reg *store.Registry, gid, flagHub string) (string, *adapter.HubConfig, error) {
	hubs, def := adapter.LoadHubs(stateRoot())
	if len(hubs) == 0 {
		return "", nil, fmt.Errorf("no hub configured (aimem hub add <name> <url> <token>)")
	}
	configured := strings.Join(slices.Sorted(maps.Keys(hubs)), ", ")
	name := flagHub
	if name == "" {
		bound := map[string]bool{}
		for _, b := range groupBindings(reg, gid) {
			if b == "" {
				b = def
			}
			bound[b] = true
		}
		switch names := slices.Sorted(maps.Keys(bound)); {
		case len(names) == 1:
			name = names[0]
		case len(names) == 0 && len(hubs) == 1:
			name = slices.Collect(maps.Keys(hubs))[0]
		case len(names) == 0:
			return "", nil, fmt.Errorf("no bound project here declares %s; name the hub with --hub (configured: %s)", gid, configured)
		default:
			return "", nil, fmt.Errorf("the projects declaring %s are bound to hubs %s; name one with --hub", gid, strings.Join(names, ", "))
		}
	}
	h, ok := hubs[name]
	if !ok {
		return "", nil, fmt.Errorf("no hub named %q (configured: %s)", name, configured)
	}
	if h.URL == "" || h.Token == "" {
		return "", nil, fmt.Errorf("hub %q has no API URL and token: publish, adopt and the comparison go over the hub API, not ssh", name)
	}
	return name, h, nil
}

func groupMetaPath(gid, key string) string {
	return "/v1/projects/" + url.PathEscape(gid) + "/meta/" + url.PathEscape(key)
}

func hubMeta(h *adapter.HubConfig, gid, key string) (string, error) {
	resp, err := hubDo(h, groupMetaTimeout, "GET", groupMetaPath(gid, key), nil, "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var b struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&b); err != nil {
		return "", fmt.Errorf("hub answer for %s: %w", key, err)
	}
	return b.Value, nil
}

func putHubMeta(h *adapter.HubConfig, gid, key, value string) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	resp, err := hubDo(h, groupMetaTimeout, "PUT", groupMetaPath(gid, key), nil, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// groupMetaTimeout bounds one meta request: a person waits on it, and a
// hub that accepts the connection and never answers must not hold an
// edit for the sync's minutes.
var groupMetaTimeout = 10 * time.Second

// unreachable is an error that is not the hub's answer: the request did
// not reach the hub or got no answer in time. The remaining keys would
// meet the same wall, so the loops stop instead of waiting on each.
func unreachable(err error) bool {
	var he *hubError
	return err != nil && !errors.As(err, &he)
}

// stopKeys reports where a loop over the charter keys stopped and
// returns the number of keys that did not land: this one and the rest.
func stopKeys(w io.Writer, k string, i int, err error) int {
	fmt.Fprintf(w, "  %s: %v; this and the remaining keys were not tried\n", k, err)
	return len(store.GroupConfigKeys) - i
}

// noSuchGroup is the hub's deliberate "no such store" answer
// (store.CodeNoSuchProject), never its text.
func noSuchGroup(err error) bool {
	var he *hubError
	return errors.As(err, &he) && he.code == store.CodeNoSuchProject
}

// checkCharterValue holds a value taken from the hub to the rules the
// hub's own PUT applies, before it replaces the local one.
func checkCharterValue(key, v string) error {
	switch key {
	case "chapters":
		return store.CheckChapters(v)
	case "policy":
		if v != "all" && v != "domain" {
			return fmt.Errorf("policy must be all or domain, the hub holds %q", v)
		}
	case "features":
		var fs []string
		if json.Unmarshal([]byte(v), &fs) != nil {
			return fmt.Errorf("features must be a JSON list of names")
		}
	}
	return nil
}

func filledOrReplaced(before string) string {
	if before == "" {
		return "filled"
	}
	return "replaced"
}

// describeChange says what a write replaced in a form a person can
// check afterwards: chapter names, short values, and only the length of
// the free-text charter.
func describeChange(key, from, to string) string {
	switch key {
	case "chapters":
		return fmt.Sprintf("chapters %s -> %s", chapterNames(from), chapterNames(to))
	case "about":
		return fmt.Sprintf("%d -> %d characters", utf8.RuneCountInString(from), utf8.RuneCountInString(to))
	}
	if from == "" {
		return fmt.Sprintf("(empty) -> %q", to)
	}
	return fmt.Sprintf("%q -> %q", from, to)
}

func chapterNames(raw string) string {
	var chs []curate.Chapter
	if raw == "" || json.Unmarshal([]byte(raw), &chs) != nil {
		return "(none)"
	}
	names := make([]string, len(chs))
	for i, c := range chs {
		names[i] = strconv.Quote(c.Name)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// charterView is the same shape `aimem group <name>` prints, from raw
// meta values; an unset key stays empty so a difference is visible.
func charterView(m map[string]string) map[string]any {
	var chs []curate.Chapter
	_ = json.Unmarshal([]byte(m["chapters"]), &chs)
	var fs []string
	_ = json.Unmarshal([]byte(m["features"]), &fs)
	return map[string]any{"about": m["about"], "policy": m["policy"], "chapters": chs, "features": fs}
}
