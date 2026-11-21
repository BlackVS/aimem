package curate

import (
	"errors"
	"os"
	"strconv"
	"time"

	"aimem/internal/embed"
	"aimem/internal/store"
)

// Backfill embeds every live memory of db that has no vector under key,
// batch texts per request, and returns how many it stored and the tokens
// spent. Each batch passes the budget gate first, like any other spend.
// It records no run history; the caller meters the spend.
func Backfill(reg *store.Registry, db *store.DB, e Embedder, key string, batch int) (n int, tokens int64, err error) {
	if batch <= 0 {
		batch = 64
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
			proj := Projection{In: int64(len(targets)) * 150}
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
		for i, t := range targets {
			if i >= len(vecs) {
				break
			}
			if err := db.SetEmbedding(t.ID, key, len(vecs[i]), embed.Encode(vecs[i])); err != nil {
				return n, tokens, err
			}
			n++
		}
		if len(vecs) < len(targets) {
			return n, tokens, errShortEmbed
		}
	}
}

var errShortEmbed = errors.New("the embedding endpoint returned fewer vectors than texts")
