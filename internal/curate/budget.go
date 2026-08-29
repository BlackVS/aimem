package curate

// Token/USD budgets for knowledge maintenance (paid-API brake). Caps are
// enforced strictly BEFORE spending: a run is refused when the window's
// recorded usage plus a worst-case projection of the run itself would
// cross the cap — the budget can be left partly unused, never overrun.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"aimem/internal/store"
)

// Cap is one window's limit. Dimensions are independent and any may be
// set: combined tokens, input tokens, output tokens (in/out usually have
// different prices and limits), or USD.
type Cap struct {
	Tokens    int64   `json:"tokens,omitempty"`     // in+out combined
	TokensIn  int64   `json:"tokens_in,omitempty"`  // input only
	TokensOut int64   `json:"tokens_out,omitempty"` // output only
	USD       float64 `json:"usd,omitempty"`
}

// Budget is the stored budget config (meta key "budget"). A project-level
// budget overrides the global one (on the user DB) entirely; a nil window
// cap means unlimited for that window. Epoch restarts counting without
// touching history.
type Budget struct {
	Daily   *Cap   `json:"daily,omitempty"`
	Weekly  *Cap   `json:"weekly,omitempty"`
	Monthly *Cap   `json:"monthly,omitempty"`
	Epoch   string `json:"epoch,omitempty"`
}

const budgetMetaKey = "budget"

// Empty reports whether no window is capped.
func (b *Budget) Empty() bool {
	return b == nil || (b.Daily == nil && b.Weekly == nil && b.Monthly == nil)
}

// LoadBudget returns the effective budget for a project and whether it is
// the project's own (true) or the global one from the user DB (false).
func LoadBudget(reg *store.Registry, projectDB *store.DB) (*Budget, bool, error) {
	if raw, err := projectDB.GetMeta(budgetMetaKey); err != nil {
		return nil, false, err
	} else if raw != "" {
		var b Budget
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, false, fmt.Errorf("project budget: %w", err)
		}
		if !b.Empty() || b.Epoch != "" {
			return &b, true, nil
		}
	}
	udb, err := reg.Open(store.UserScopeProject)
	if err != nil {
		return nil, false, err
	}
	raw, err := udb.GetMeta(budgetMetaKey)
	if err != nil || raw == "" {
		return nil, false, err
	}
	var b Budget
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, false, fmt.Errorf("global budget: %w", err)
	}
	return &b, false, nil
}

// SaveBudget stores a budget on the given DB ("" value clears it).
func SaveBudget(db *store.DB, b *Budget) error {
	if b == nil {
		return db.SetMeta(budgetMetaKey, "")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return db.SetMeta(budgetMetaKey, string(raw))
}

// WindowStart returns the UTC calendar start of a budget window, moved
// forward to the budget epoch if that is later (reset semantics).
func WindowStart(window string, now time.Time, epoch string) string {
	now = now.UTC()
	var start time.Time
	switch window {
	case "daily":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "weekly":
		// ISO week: Monday 00:00 UTC.
		wd := (int(now.Weekday()) + 6) % 7
		day := now.AddDate(0, 0, -wd)
		start = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	case "monthly":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		start = now
	}
	s := start.Format(time.RFC3339)
	if epoch > s {
		return epoch
	}
	return s
}

// Projection is the worst-case cost of the run about to happen.
type Projection struct {
	In     int64
	Out    int64
	USD    float64 // 0 when prices are unknown
	Priced bool    // whether USD is meaningful
}

func (p Projection) Tokens() int64 { return p.In + p.Out }

// ProjectRun bounds the next run's cost from the run caps: every event is
// clipped in buildPrompt (600+1200 chars ≈ 450 tokens) plus prompt/JSON
// overhead, and the completion is at most ~maxFacts sentences.
func ProjectRun(maxEvents, maxFacts int) Projection {
	p := Projection{In: int64(maxEvents)*500 + 600, Out: int64(maxFacts)*80 + 200}
	// Prices in USD per 1M tokens; needed only for USD caps.
	pin, err1 := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_IN"), 64)
	pout, err2 := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_OUT"), 64)
	if err1 == nil && err2 == nil {
		p.USD = float64(p.In)/1e6*pin + float64(p.Out)/1e6*pout
		p.Priced = true
	}
	return p
}

// WindowUsage sums recorded usage for one window. Global budgets count
// every project in the registry; project budgets count only their own.
func WindowUsage(reg *store.Registry, projectDB *store.DB, global bool,
	window string, now time.Time, epoch string) (inTok, outTok int64, usd float64, err error) {
	since := WindowStart(window, now, epoch)
	dbs := []*store.DB{projectDB}
	if global {
		ids, err := reg.Projects()
		if err != nil {
			return 0, 0, 0, err
		}
		dbs = dbs[:0]
		for _, id := range ids {
			db, err := reg.Open(id)
			if err != nil {
				continue
			}
			dbs = append(dbs, db)
		}
	}
	// Per-run so unpriced runs (cost 0, tokens > 0) can be charged at the
	// configured prices — usage must never undercount against a USD cap.
	pin, _ := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_IN"), 64)
	pout, _ := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_OUT"), 64)
	// Budgets are machine-local, so usage must be too: run history synced
	// or imported from other hosts (e.g. a project migrated between hubs
	// carrying its cost records) is provenance, not this machine's spend.
	self, _ := os.Hostname()
	for _, db := range dbs {
		runs, err := db.CurateRuns()
		if err != nil {
			return 0, 0, 0, err
		}
		for _, r := range runs {
			if r.TS < since {
				continue
			}
			if r.Host != "" && self != "" && r.Host != self {
				continue
			}
			inTok += r.InputTokens
			outTok += r.OutputTokens
			if r.CostUSD > 0 {
				usd += r.CostUSD
			} else {
				usd += float64(r.InputTokens)/1e6*pin + float64(r.OutputTokens)/1e6*pout
			}
		}
	}
	return inTok, outTok, usd, nil
}

// CheckBudget reports whether the projected run fits every capped window.
// A USD cap with unknown prices refuses the run outright — silence must
// never turn into overspend.
func CheckBudget(reg *store.Registry, projectDB *store.DB, b *Budget,
	global bool, proj Projection, now time.Time) error {
	if b.Empty() {
		return nil
	}
	for _, wc := range []struct {
		name string
		cap  *Cap
	}{{"daily", b.Daily}, {"weekly", b.Weekly}, {"monthly", b.Monthly}} {
		if wc.cap == nil {
			continue
		}
		inTok, outTok, usd, err := WindowUsage(reg, projectDB, global, wc.name, now, b.Epoch)
		if err != nil {
			return err
		}
		if wc.cap.Tokens > 0 && inTok+outTok+proj.Tokens() > wc.cap.Tokens {
			return fmt.Errorf("budget exhausted: %s cap %d tokens, used %d, next run may need %d (aimem budget to inspect, --force to bypass)",
				wc.name, wc.cap.Tokens, inTok+outTok, proj.Tokens())
		}
		if wc.cap.TokensIn > 0 && inTok+proj.In > wc.cap.TokensIn {
			return fmt.Errorf("budget exhausted: %s input cap %d tokens, used %d in, next run may need %d (aimem budget to inspect, --force to bypass)",
				wc.name, wc.cap.TokensIn, inTok, proj.In)
		}
		if wc.cap.TokensOut > 0 && outTok+proj.Out > wc.cap.TokensOut {
			return fmt.Errorf("budget exhausted: %s output cap %d tokens, used %d out, next run may need %d (aimem budget to inspect, --force to bypass)",
				wc.name, wc.cap.TokensOut, outTok, proj.Out)
		}
		if wc.cap.USD > 0 {
			if !proj.Priced {
				return fmt.Errorf("budget: %s cap is in USD but AIMEM_PRICE_IN/AIMEM_PRICE_OUT are unset — refusing to spend unpriced (set prices per 1M tokens, or use a token cap)", wc.name)
			}
			if usd+proj.USD > wc.cap.USD {
				return fmt.Errorf("budget exhausted: %s cap $%.2f, used $%.4f, next run may cost $%.4f (aimem budget to inspect, --force to bypass)",
					wc.name, wc.cap.USD, usd, proj.USD)
			}
		}
	}
	return nil
}
