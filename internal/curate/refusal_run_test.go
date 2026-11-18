package curate

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"aimem/internal/llmrate"
	"aimem/internal/store"
)

// refusingEmbedder answers every call with a refusal of the account, or
// with an ordinary failure.
type refusingEmbedder struct{ err error }

func (f *refusingEmbedder) Embed(texts []string) ([][]float32, int64, error) {
	return nil, 0, f.err
}

// Extraction succeeds and the dedup embedding is refused for the
// account: the run stops with the Refusal, writes nothing and keeps its
// cursor, so the command can stop. Any other embedding failure still
// only skips dedup, as before.
func TestRunStopsAtARefusedDedupEmbedding(t *testing.T) {
	refusal := llmrate.Refused(402, []byte(`{"error":{"message":"Insufficient credits"}}`))
	for _, c := range []struct {
		name    string
		err     error
		refused bool
	}{
		{"refused", refusal, true},
		{"an ordinary failure", errors.New("embeddings error (HTTP 400): bad input"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			reg, err := store.NewRegistry(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reg.Close()
			seed(t, reg, "proj-rf", 2)
			rep, err := Run(reg, root, "proj-rf", &fakeExtractor{}, RunOpts{
				MaxFacts: 10, MaxEvents: 100, Embedder: &refusingEmbedder{err: c.err}, EmbedModel: "fake-embed", DedupSim: 0.9})
			_, refused := llmrate.AsRefusal(err)
			if refused != c.refused {
				t.Fatalf("refused %v, want %v (err %v)", refused, c.refused, err)
			}
			db, _ := reg.Open("proj-rf")
			mems, _ := db.Memories(true)
			_, cursorErr := os.Stat(cursorPath(root, "proj-rf"))
			if c.refused && (rep != nil || len(mems) != 0 || cursorErr == nil) {
				t.Fatalf("a refused run wrote %d memories or moved its cursor", len(mems))
			}
			if !c.refused && (err != nil || len(mems) == 0) {
				t.Fatalf("an ordinary embedding failure stopped the run: %v, %d memories", err, len(mems))
			}
		})
	}
}

// A refusal stays recorded through a later failed call, even one whose
// error body decodes, and is cleared by the next completion that
// returns an answer.
func TestCompleteClearsTheRefusalOnlyOnSuccess(t *testing.T) {
	reset := ladder(t)
	reset()
	answers := []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"no credits","type":"insufficient_quota"}}`},
		{400, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`},
		{200, `{"error":{"message":"proxy says no"}}`},
		{200, `{"choices":[{"message":{"content":"fine"}}]}`},
	}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[n.Add(1)-1]
		w.WriteHeader(a.status)
		w.Write([]byte(a.body))
	}))
	defer srv.Close()
	x := &OpenAIExtractor{BaseURL: srv.URL, Model: "m"}
	for i, wantKept := range []bool{true, true, true, false} {
		_, _, err := x.Complete("p")
		_, kept := llmrate.Status()["last_refusal"]
		if kept != wantKept || (i < 3) != (err != nil) {
			t.Fatalf("answer %d: err %v, refusal kept %v, want %v", i, err, kept, wantKept)
		}
	}
}
