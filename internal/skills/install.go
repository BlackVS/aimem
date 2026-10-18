package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aimem/internal/process"
)

// Installing: each picked skill is written whole into each target
// directory (the client's skills directory, as the aiskills installers
// write it), replacing a directory of the same name, and the installers'
// record beside it (.ai-skills.json) is updated with the version and
// source per skill, so the bootstrap's version check and either installer
// agree on what is there.

// Target is one skills directory to install into.
type Target struct {
	Client string
	Dir    string
}

// Installed is one skill written into one target.
type Installed struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Source   string `json:"source"`
	Client   string `json:"client"`
	Dir      string `json:"dir"`
	Replaced bool   `json:"replaced"` // a directory of that name was there
}

// Install writes picks into targets from the sets' files (by digest) and
// updates each target's record. It stops at the first failure, with what
// was installed before it.
func Install(targets []Target, picks []Skill, files func(digest string) (map[string][]byte, error), now time.Time) ([]Installed, error) {
	var out []Installed
	for _, t := range targets {
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			return out, err
		}
		rec := process.ReadInstalledRecord(t.Dir)
		if rec == nil {
			rec = &process.InstalledRecord{}
		}
		if rec.Versions == nil {
			rec.Versions = map[string]string{}
		}
		if rec.Sources == nil {
			rec.Sources = map[string]string{}
		}
		for _, p := range picks {
			all, err := files(p.Digest)
			if err != nil {
				return out, err
			}
			own, err := FilesOf(all, []string{p.Name})
			if err != nil {
				return out, err
			}
			replaced, err := writeSkill(t.Dir, p.Name, own)
			if err != nil {
				return out, fmt.Errorf("%s into %s: %w", p.Name, t.Dir, err)
			}
			rec.Versions[p.Name], rec.Sources[p.Name] = p.Version, p.Source
			if !has(rec.Skills, p.Name) {
				rec.Skills = append(rec.Skills, p.Name)
			}
			out = append(out, Installed{Name: p.Name, Version: p.Version, Source: p.Source, Client: t.Client, Dir: filepath.Join(t.Dir, p.Name), Replaced: replaced})
		}
		sort.Strings(rec.Skills)
		rec.Version = recordVersion(rec, picks)
		rec.InstalledAt = now.UTC().Format(time.RFC3339)
		if err := writeRecord(t.Dir, rec); err != nil {
			return out, err
		}
	}
	return out, nil
}

// recordVersion is the record's one version: the release's when a release
// skill is there, else the instance's; the per-skill map is the truth.
func recordVersion(rec *process.InstalledRecord, picks []Skill) string {
	for _, p := range picks {
		if p.Source == SourceRelease {
			return p.Version
		}
	}
	if rec.Version != "" {
		return rec.Version
	}
	if len(picks) > 0 {
		return picks[0].Version
	}
	return ""
}

// writeSkill writes the skill's files into dir/name whole: a temporary
// directory first, the old directory set aside, one rename, the old one
// removed last; a rename that fails puts the old one back.
func writeSkill(dir, name string, own map[string][]byte) (bool, error) {
	tmp, err := os.MkdirTemp(dir, ".tmp-"+name+"-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	for p, data := range own {
		rel := strings.TrimPrefix(p, name+"/")
		fp := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(fp, data, 0o644); err != nil {
			return false, err
		}
	}
	dest := filepath.Join(dir, name)
	_, statErr := os.Stat(dest)
	replaced := statErr == nil
	aside := tmp + ".old"
	if replaced {
		if err := os.Rename(dest, aside); err != nil {
			return false, err
		}
		defer os.RemoveAll(aside)
	}
	if err := os.Rename(tmp, dest); err != nil {
		if replaced {
			os.Rename(aside, dest)
		}
		return false, err
	}
	return replaced, nil
}

func writeRecord(dir string, rec *process.InstalledRecord) error {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, process.InstalledRecordName+".tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, process.InstalledRecordName))
}
