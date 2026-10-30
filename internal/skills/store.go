package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The store is file-based under the state root, on the hub and on a
// member alike: root/skills/sets/<digest>/ holds one set's files, complete
// when its marker exists (written last, after a rename); on the hub,
// root/skills/index.json names per source the current import and the
// kept ones, replaced whole. It is hub state: a backup or a migration of
// the state root carries it. The hub has no hub-wide table, and a skills
// release is one per hub, not one per project.

// Kept is how many imports per source the hub keeps, the current one
// included: an admin re-imports an earlier release without the archive.
const Kept = 5

const completeMarker = ".complete"

// Store is the skills store under one state root.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore returns the store under root.
func NewStore(root string) *Store { return &Store{root: root} }

// Dir is the store's directory.
func (s *Store) Dir() string { return filepath.Join(s.root, "skills") }

// IndexPath is the hub's index file.
func (s *Store) IndexPath() string { return filepath.Join(s.Dir(), "index.json") }

// SetDir is where the set with that digest lives.
func SetDir(root, digest string) string {
	return filepath.Join(root, "skills", "sets", digest)
}

// Index is the hub's record of its imports, per source.
type Index struct {
	Sources map[string]*SourceState `json:"sources"`
}

// SourceState is one source's current import and the kept ones, newest
// first; the current one is the first kept.
type SourceState struct {
	Current *Entry  `json:"current,omitempty"`
	Kept    []Entry `json:"kept"`
}

// Current maps each source with an import to its current entry.
func (ix *Index) Current() map[string]*Entry {
	out := map[string]*Entry{}
	for src, st := range ix.Sources {
		if st != nil && st.Current != nil {
			out[src] = st.Current
		}
	}
	return out
}

// Effective is the merged set of the current imports.
func (ix *Index) Effective() []Skill { return Effective(ix.Current()) }

// Load reads the index. No index means nothing was imported: an empty
// index, not an error. An index that exists and cannot be read is a
// fault, reported as such: it is not "no skills".
func (s *Store) Load() (*Index, error) {
	ix := &Index{Sources: map[string]*SourceState{}}
	raw, err := os.ReadFile(s.IndexPath())
	if errors.Is(err, fs.ErrNotExist) {
		return ix, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skills index %s: %w", s.IndexPath(), err)
	}
	if err := json.Unmarshal(raw, ix); err != nil {
		return nil, fmt.Errorf("skills index %s is not readable: %w (restore it from a backup of the state root, or re-import each source)", s.IndexPath(), err)
	}
	if ix.Sources == nil {
		ix.Sources = map[string]*SourceState{}
	}
	return ix, nil
}

// Import stores set under its digest and makes it the current import of
// its source, keeping the last Kept per source and removing the files of
// sets no source keeps any more. The set is validated first; a set that
// does not validate stores nothing. The second value says whether the
// files were written now (false for a digest already stored).
func (s *Store) Import(set *Set, by string, now time.Time) (Entry, bool, error) {
	if err := set.Validate(); err != nil {
		return Entry{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ix, err := s.Load()
	if err != nil {
		return Entry{}, false, err
	}
	digest := set.Digest()
	written, err := s.put(set.Files, digest)
	if err != nil {
		return Entry{}, false, err
	}
	e := Entry{Version: set.Version, Digest: digest, Skills: set.Names(), Bytes: set.Bytes(), ImportedAt: now.UTC().Format(time.RFC3339), ImportedBy: by}
	st := ix.Sources[set.Source]
	if st == nil {
		st = &SourceState{}
		ix.Sources[set.Source] = st
	}
	// An import is a version with a content: the same version with the
	// same content replaces its entry; the same content under another
	// version (a release that changed only its installer) is another kept
	// import sharing the stored files, so a pin on either version resolves.
	kept := []Entry{e}
	for _, k := range st.Kept {
		if k.Digest != digest || k.Version != set.Version {
			kept = append(kept, k)
		}
	}
	if len(kept) > Kept {
		kept = kept[:Kept]
	}
	st.Current, st.Kept = &kept[0], kept
	if err := s.writeIndex(ix); err != nil {
		return Entry{}, false, err
	}
	s.prune(ix)
	return e, written, nil
}

func (s *Store) writeIndex(ix *Index) error {
	raw, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		return err
	}
	tmp := s.IndexPath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.IndexPath())
}

// prune removes the set directories no source keeps. A directory in use
// by a read that started before the index changed may vanish under it;
// that read fails and the next one reads the new index.
func (s *Store) prune(ix *Index) {
	keep := map[string]bool{}
	for _, st := range ix.Sources {
		if st == nil {
			continue
		}
		for _, k := range st.Kept {
			keep[k.Digest] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir(), "sets"))
	if err != nil {
		return
	}
	s.sweepTemp(entries)
	for _, d := range entries {
		if ValidDigest(d.Name()) && !keep[d.Name()] {
			os.RemoveAll(filepath.Join(s.Dir(), "sets", d.Name()))
		}
	}
}

// tempMaxAge is how old a temporary directory under sets must be before
// housekeeping removes it; a younger one may still be filling.
const tempMaxAge = time.Hour

// sweepTemp removes the temporary directories an interrupted put left
// under sets (a crash between MkdirTemp and the rename). It goes by the
// directory's modification time, so another process's put in progress
// is left alone; a digest-named entry is never one of them.
func (s *Store) sweepTemp(entries []os.DirEntry) {
	for _, d := range entries {
		if !strings.HasPrefix(d.Name(), ".tmp-") {
			continue
		}
		if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > tempMaxAge {
			os.RemoveAll(filepath.Join(s.Dir(), "sets", d.Name()))
		}
	}
}

// Put stores files under digest as a complete entry, atomically: files
// into a temporary directory, the marker last, one rename. The digest
// must be the files' own. It is the member's cache write, after the
// member verified the hub's answer; on the hub, Import's storage step.
func (s *Store) Put(files map[string][]byte, digest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.put(files, digest)
}

func (s *Store) put(files map[string][]byte, digest string) (bool, error) {
	if !ValidDigest(digest) {
		return false, errors.New("digest must be 64 lowercase hex characters")
	}
	if got := (&Set{Files: files}).Digest(); got != digest {
		return false, fmt.Errorf("the files digest to %s, not %s", got[:12], digest[:12])
	}
	for p := range files {
		if err := checkPath(p); err != nil {
			return false, fmt.Errorf("file %q: %w", p, err)
		}
	}
	dir := SetDir(s.root, digest)
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+digest[:12]+"-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	for p, data := range files {
		fp := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return false, err
		}
		if err := os.WriteFile(fp, data, 0o600); err != nil {
			return false, err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), []byte(digest+"\n"), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, err2 := os.Stat(filepath.Join(dir, completeMarker)); err2 != nil {
			return false, err
		}
		return false, nil // another writer finished the same digest first
	}
	return true, nil
}

// Read returns the files stored under digest, complete and digesting to
// it; an entry that is missing, incomplete or damaged is an error.
func (s *Store) Read(digest string) (map[string][]byte, error) {
	if !ValidDigest(digest) {
		return nil, errors.New("digest must be 64 lowercase hex characters")
	}
	dir := SetDir(s.root, digest)
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err != nil {
		return nil, fmt.Errorf("set %s is not in the store", digest[:12])
	}
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == completeMarker {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("set %s: %w", digest[:12], err)
	}
	if got := (&Set{Files: files}).Digest(); got != digest {
		return nil, fmt.Errorf("set %s is damaged on disk: its files digest to %s", digest[:12], got[:12])
	}
	return files, nil
}

// Has reports whether a complete entry for digest exists.
func (s *Store) Has(digest string) bool {
	_, err := os.Stat(filepath.Join(SetDir(s.root, digest), completeMarker))
	return err == nil
}

// Remove deletes the entry for digest, complete or not: the member's
// repair of a cache entry that no longer digests to its name.
func (s *Store) Remove(digest string) error {
	if !ValidDigest(digest) {
		return errors.New("digest must be 64 lowercase hex characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(SetDir(s.root, digest))
}

// Trim keeps at most n complete entries on a member, the newest by
// modification time: a member never imports, so nothing else bounds its
// cache. A set the index keeps is never removed (the admin's machine is
// the hub host and a member in one root), and an index that cannot be
// read trims nothing: that root is the hub's, and in a fault.
func (s *Store) Trim(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ix, err := s.Load()
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, st := range ix.Sources {
		if st == nil {
			continue
		}
		for _, k := range st.Kept {
			kept[k.Digest] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir(), "sets"))
	if err != nil {
		return
	}
	s.sweepTemp(entries)
	type aged struct {
		name string
		mod  time.Time
	}
	var all []aged
	for _, d := range entries {
		if !ValidDigest(d.Name()) || kept[d.Name()] {
			continue
		}
		fi, err := os.Stat(filepath.Join(s.Dir(), "sets", d.Name(), completeMarker))
		if err != nil {
			continue
		}
		all = append(all, aged{d.Name(), fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for i := n; i < len(all); i++ {
		os.RemoveAll(filepath.Join(s.Dir(), "sets", all[i].name))
	}
}

// FilesOf returns the files of the named skills from a set's files, keyed
// as the set keys them; a name the set lacks is an error.
func FilesOf(files map[string][]byte, skills []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, n := range skills {
		found := false
		for p, data := range files {
			if strings.HasPrefix(p, n+"/") {
				out[p] = data
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("the set has no skill %s", n)
		}
	}
	return out, nil
}
