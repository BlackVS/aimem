package curate

import (
	"errors"
	"os"
	"strconv"
	"time"

	"aimem/internal/store"
)

// Backfill embeds every live memory of db that has no vector under key,
// batch texts per request, and returns how many it stored and the tokens
// spent. Each batch passes db's budget gate with the tokens this call has
// already spent added to its projection, so a long backfill cannot pass
// the cap batch by batch. The spend is recorded once, as one row of db's
// run history under meterModel, when the call ends, on an error too;
// callers record none of it again.
func Backfill(reg *store.Registry, db *store.DB, e Embedder, key, meterModel string, batch int) (n int, tokens int64, err error) {
	if batch <= 0 {
		batch = 64
	}
	host, _ := os.Hostname()
	defer func() {
		if tokens > 0 || n > 0 {
			_ = db.AddCurateRun(&store.CurateRun{
				TS: time.Now().UTC().Format(time.RFC3339), Host: host,
				Model: meterModel, Written: n, InputTokens: tokens,
			})
		}
	}()
	// Once per call: a key already holding vectors of several lengths
	// is refused before anything is spent (the per-batch check reads one
	// row only).
	if _, err := db.EmbedDim(key); err != nil {
		return 0, 0, err
	}
	for {
		targets, err := db.NeedingEmbedding(key, batch)
		if err != nil || len(targets) == 0 {
			return n, tokens, err
		}
		// Budget gate: worst-case ~150 tokens per fact sentence. USD
		// caps charge these at AIMEM_PRICE_IN (chat input price),
		// above real embedding prices, so never an undercount.
		if budget, owned, err := LoadBudget(reg, db); err != nil {
			return n, tokens, err
		} else if !budget.Empty() {
			proj := Projection{In: tokens + int64(len(targets))*150}
			if pin, perr := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_IN"), 64); perr == nil {
				proj.USD, proj.Priced = float64(proj.In)/1e6*pin, true
			}
			if err := CheckBudget(reg, db, budget, !owned, proj, time.Now()); err != nil {
				return n, tokens, err
			}
		}
		texts := make([]string, len(targets))
		for i, t := range targets {
			texts[i] = t.Text
		}
		vecs, used, err := e.Embed(texts)
		tokens += used
		if err != nil {
			return n, tokens, err
		}
		batchVecs := make([]store.EmbedVec, 0, len(targets))
		for i, t := range targets {
			if i >= len(vecs) {
				break
			}
			batchVecs = append(batchVecs, store.EmbedVec{MemoryID: t.ID, Vec: vecs[i]})
		}
		if err := db.SetEmbeddings(key, batchVecs); err != nil {
			return n, tokens, err
		}
		n += len(batchVecs)
		if len(vecs) < len(targets) {
			return n, tokens, errShortEmbed
		}
	}
}

var errShortEmbed = errors.New("the embedding endpoint returned fewer vectors than texts")
