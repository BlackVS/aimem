package store

import (
	"errors"
	"testing"

	"aimem/internal/embed"
)

// A change of the embedding model leaves the old key's vectors in place.
// Coverage must count only the key recall queries with.
func TestStatsCountsCoverageUnderTheGivenKeyOnly(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	var ids []string
	for _, txt := range []string{"alpha fact", "beta fact", "gamma fact"} {
		id, _, err := db.Remember(txt, "test", RememberOpts{Kind: "fact"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	vec := embed.Encode([]float32{1, 0})
	for _, id := range ids {
		if err := db.SetEmbedding(id, "old", 2, vec); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetEmbedding(ids[0], "new", 2, vec); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"old": 3, "new": 1, "": 0, "other": 0} {
		s, err := db.Stats(key)
		if err != nil {
			t.Fatal(err)
		}
		if s.Memories != 3 || s.Embedded != want || s.EmbedModel != key {
			t.Errorf("Stats(%q) = memories %d, embedded %d under %q; want 3, %d under %q",
				key, s.Memories, s.Embedded, s.EmbedModel, want, key)
		}
	}
}

// Every writer goes through SetEmbedding: a vector of another length than
// the key's stored ones is refused, a same-length one and another key's
// are not, and a dropped key accepts a new dimension.
func TestSetEmbeddingRefusesAnotherDimensionUnderOneKey(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	a, _, _ := db.Remember("first fact", "test", RememberOpts{Kind: "fact"})
	b, _, _ := db.Remember("second fact", "test", RememberOpts{Kind: "fact"})
	if err := db.SetEmbedding(a, "m", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	if err := db.SetEmbedding(b, "m", 3, embed.Encode([]float32{1, 0, 0})); !errors.Is(err, ErrEmbedDim) {
		t.Fatalf("a 3-dimensional vector under a 2-dimensional key: err = %v, want ErrEmbedDim", err)
	}
	// A fresh handle reads the stored dimension from the table, not a cache.
	r2, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r2.Close)
	db2, _ := r2.Open("proj-a")
	if err := db2.SetEmbedding(b, "m", 3, embed.Encode([]float32{1, 0, 0})); !errors.Is(err, ErrEmbedDim) {
		t.Fatalf("second handle: err = %v, want ErrEmbedDim", err)
	}
	if err := db.SetEmbedding(b, "m", 2, embed.Encode([]float32{0, 1})); err != nil {
		t.Fatalf("same dimension refused: %v", err)
	}
	if err := db.SetEmbedding(b, "other", 3, embed.Encode([]float32{1, 0, 0})); err != nil {
		t.Fatalf("another key refused: %v", err)
	}
	u, err := db.DropEmbeddings("m")
	if err != nil || u.Rows != 2 || u.Bytes != 16 {
		t.Fatalf("drop = %+v, %v; want 2 rows of 16 bytes", u, err)
	}
	if err := db.SetEmbedding(a, "m", 3, embed.Encode([]float32{1, 0, 0})); err != nil {
		t.Fatalf("after the drop the key takes a new dimension: %v", err)
	}
	ks, _ := db.EmbedKeys()
	if len(ks) != 2 || ks[0].Model != "m" || ks[0].Rows != 1 || ks[1].Model != "other" {
		t.Fatalf("keys = %+v", ks)
	}
}

// A long-running reader (the service's recall) must see a key another
// process dropped and refilled at another length, not a cached one.
func TestEmbedDimReadsAKeyRefilledElsewhere(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	a, _, _ := db.Remember("first fact", "test", RememberOpts{Kind: "fact"})
	if err := db.SetEmbedding(a, "m", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.EmbedDim("m"); d != 2 {
		t.Fatalf("dim %d, want 2", d)
	}
	other, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	odb, _ := other.Open("proj-a")
	if _, err := odb.DropEmbeddings("m"); err != nil {
		t.Fatal(err)
	}
	if err := odb.SetEmbedding(a, "m", 3, embed.Encode([]float32{1, 0, 0})); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.EmbedDim("m"); d != 3 {
		t.Fatalf("the reader kept a dropped length: dim %d, want 3", d)
	}
}
