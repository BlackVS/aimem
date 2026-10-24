package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aimem/internal/ident"
	"aimem/internal/store"
	"aimem/internal/writingrule"
)

const textCheckUsage = "usage: aimem tasks text-check [-p <project>] [--since <YYYY-MM-DD or RFC 3339 time>]"

// tasksTextCheck reports the task text a project already holds that the
// hub's readability filter would refuse today: one line per flagged task
// field or comment, then a summary. It reads through the local service's
// task routes and changes nothing; the second stage, which only the hub's
// write path asks, is not consulted, so the report is the filter's view.
func tasksTextCheck(args []string) error {
	fs := flag.NewFlagSet("tasks text-check", flag.ContinueOnError)
	p := projectFlag(fs, "project id (default: current directory's project)")
	since := fs.String("since", "", "only task fields updated and comments written at or after this date or time")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("%s", textCheckUsage)
	}
	proj := *p
	if proj == "" {
		id, err := ident.ProjectID(".")
		if err != nil {
			return err
		}
		proj = id
	}
	from, err := sinceTime(*since)
	if err != nil {
		return err
	}
	var tasks, fields, comments, flagged int
	report := func(where string, f *writingrule.Finding) {
		flagged++
		fmt.Printf("%s: %q (%s)\n", where, f.Span, f.Reason)
	}
	after := ""
	for {
		var page struct {
			Tasks []store.TaskSummary `json:"tasks"`
			Next  string              `json:"next_cursor"`
		}
		q := url.Values{"include_archived": {"true"}, "limit": {"100"}}
		if after != "" {
			q.Set("after", after)
		}
		if err := localTaskGet("/v1/projects/"+url.PathEscape(proj)+"/tasks?"+q.Encode(), &page); err != nil {
			return err
		}
		for _, summary := range page.Tasks {
			tasks++
			if from == "" || summary.UpdatedAt >= from {
				var t store.Task
				if err := localTaskGet("/v1/tasks/"+url.PathEscape(summary.ID), &t); err != nil {
					return err
				}
				for _, f := range t.TextFields() {
					fields++
					if fd := writingrule.Unreadable(f[1]); fd != nil {
						report(fmt.Sprintf("task %s %s", t.ID, f[0]), fd)
					}
				}
			}
			n, err := checkComments(summary.ID, from, report)
			if err != nil {
				return err
			}
			comments += n
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	scope := ""
	if from != "" {
		scope = " since " + from
	}
	fmt.Printf("project %s%s: %d tasks, %d fields and %d comments checked, %d flagged (the filter alone; nothing was changed)\n",
		proj, scope, tasks, fields, comments, flagged)
	return nil
}

// checkComments checks a task's comments written at or after from.
func checkComments(task, from string, report func(string, *writingrule.Finding)) (int, error) {
	checked := 0
	var after int64
	for {
		var page struct {
			Comments []store.TaskComment `json:"comments"`
			Next     int64               `json:"next_cursor"`
		}
		path := fmt.Sprintf("/v1/tasks/%s/comments?limit=100&after=%d", url.PathEscape(task), after)
		if err := localTaskGet(path, &page); err != nil {
			return checked, err
		}
		for _, c := range page.Comments {
			if from != "" && c.CreatedAt < from {
				continue
			}
			checked++
			if fd := writingrule.Unreadable(c.Body); fd != nil {
				report(fmt.Sprintf("task %s comment %d by %s at %s", task, c.Sequence, c.Actor.Name, c.CreatedAt), fd)
			}
		}
		if page.Next == 0 {
			return checked, nil
		}
		after = page.Next
	}
}

// sinceTime normalises --since to the RFC 3339 UTC form the hub stores,
// so a string comparison orders it against stored times.
func sinceTime(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("--since %q: want a date (2026-10-01) or an RFC 3339 time (2026-10-01T08:00:00Z)", v)
}

// localTaskGet reads one task route of the local service; a refusal
// carries the hub's own words.
func localTaskGet(path string, into any) error {
	resp, err := client().Get("http://aimem" + path)
	if err != nil {
		return fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, into)
}
