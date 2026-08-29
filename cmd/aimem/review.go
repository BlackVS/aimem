package main

// `aimem review` — the staleness review loop (FEATURE-PROPOSALS #5).
// Lists active, unpinned facts that are old, thinly corroborated, and
// untouched since; each is then confirmed here, superseded with
// `aimem supersede`, or expired with `aimem forget`. The queue is
// derived from the audit trail — reviewing IS what empties it.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"aimem/internal/ident"
	"aimem/internal/store"
)

func reviewCmd(args []string) error {
	if len(args) > 0 && args[0] == "confirm" {
		fs := flag.NewFlagSet("review confirm", flag.ExitOnError)
		p := fs.String("p", "", "project id (default: derived from the current directory)")
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: aimem review confirm [-p <project>] <memory-id>")
		}
		proj, err := reviewProject(*p)
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		if err := postJSONQuiet("/v1/projects/"+url.PathEscape(proj)+"/memories/"+url.PathEscape(fs.Arg(0))+"/confirm",
			map[string]any{"actor": host + "/review"}); err != nil {
			return err
		}
		fmt.Printf("confirmed %s (audited; back in the queue after the age window)\n", fs.Arg(0))
		return nil
	}

	fs := flag.NewFlagSet("review", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: derived from the current directory)")
	days := fs.Int("days", store.DefaultReviewAgeDays, "age window: only facts untouched this long")
	maxCorr := fs.Int("max-corroboration", store.DefaultReviewMaxCorroboration, "only facts with at most this many sources")
	limit := fs.Int("limit", 50, "maximum queue entries")
	fs.Parse(args)
	proj, err := reviewProject(*p)
	if err != nil {
		return err
	}
	resp, err := client().Get(fmt.Sprintf("http://aimem/v1/projects/%s/memories/review?days=%d&max_corroboration=%d&limit=%d",
		url.PathEscape(proj), *days, *maxCorr, *limit))
	if err != nil {
		return fmt.Errorf("%w (is `aimem serve` running?)", err)
	}
	defer resp.Body.Close()
	var res struct {
		Items  []store.ReviewItem `json:"items"`
		Cutoff string             `json:"cutoff"`
		Error  string             `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("%s", res.Error)
	}
	if len(res.Items) == 0 {
		fmt.Printf("review queue for %s is clear (window %dd, corroboration <= %d)\n", proj, *days, *maxCorr)
		return nil
	}
	fmt.Printf("%d fact(s) in %s unreviewed since %s — confirm / supersede / forget each:\n\n",
		len(res.Items), proj, res.Cutoff[:10])
	for _, it := range res.Items {
		age := ""
		if t, err := time.Parse(time.RFC3339, it.LastSeen); err == nil {
			age = fmt.Sprintf("%dd", int(time.Since(t).Hours()/24))
		}
		fmt.Printf("%s  %-10s conf %.2f  corr %d  last seen %s (%s)\n  %s\n\n",
			it.ID, it.Kind, it.Confidence, it.Corroboration, it.LastSeen[:10], age, clipText(it.Text, 160))
	}
	fmt.Println("verdicts: aimem review confirm <id> | aimem supersede -p " + proj + " --id <id> \"new text\" | aimem forget -p " + proj + " --id <id>")
	return nil
}

func reviewProject(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return ident.ProjectID(".")
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
