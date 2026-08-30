// Package tui is the read-only operator dashboard (`aimem tui`): projects,
// sessions, background-job state, curation cost, DB/embedding stats,
// groups, hub connectivity. It reads the same store/config the service
// uses; it never writes.
package tui

import (
	"bytes"
	"cmp"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"aimem/internal/adapter"
	"aimem/internal/curate"
	"aimem/internal/ident"
	"aimem/internal/store"
)

type usage struct{ In, Out int64 }

func (u usage) total() int64 { return u.In + u.Out }
func (u *usage) add(o usage) { u.In += o.In; u.Out += o.Out }

type projectRow struct {
	ID     string
	Stats  store.ProjectStats
	Groups []string
	Hub    string           // declared hub binding ("" = machine default)
	Curate *store.CurateRun // newest run, nil if never
	Today  usage            // since UTC midnight
	Week   usage            // last 7 days
	Month  usage            // last 30 days
}

type modelUsage struct{ Today, Week, Month usage }

// chapterCount is one knowledge-base chapter with its current fact count.
type chapterCount struct {
	Name  string
	Count int
}

// groupSession is one session inside a group's member project, for the
// Groups tab (who is actually feeding this shared scope).
type groupSession struct {
	Project   string
	SessionID string
	Client    string
	Events    int
	LastTS    string
}

type budgetLine struct {
	Window  string
	UsedIn  int64
	UsedOut int64
	UsedUSD float64
	Cap     curate.Cap
}

type snapshot struct {
	When        time.Time
	Projects    []projectRow
	Budget      []budgetLine              // global budget windows with usage
	Models      map[string]*modelUsage    // curation usage per model
	Groups      map[string][]string       // group id -> declaring project ids
	GroupFacts  map[string][]string       // group id -> newest fact texts
	GroupSess   map[string][]groupSession // group id -> newest member sessions
	GroupAbout  map[string]string         // group id -> charter ("about" meta)
	GroupPolicy map[string]string         // group id -> promotion policy meta
	GroupChaps  map[string][]chapterCount // group id -> declared chapters with fact counts
	CurateModel string
	EmbedModel  string
	Hub         string              // default hub URL or "" (summary strip)
	HubRes      map[string]any      // default hub /v1/health resources (best-effort)
	HubProjects int                 // project count reported by default hub health
	HubOK       bool                // default hub reachable this refresh
	Hubs        []hubLine           // every configured hub, for the Hub tab
	Spool       int                 // queued events, all spools summed
	SpoolBy     map[string]int      // spool label ("local service", "hub <name>") -> queued events
	Conflicts   []string            // bound files with a pending .merge preview on THIS machine
	Timers      []string            // systemd timer lines (best-effort)
	Tail        []store.StoredEvent // latest events of selected project
	Err         error
}

const (
	viewProjects = iota
	viewGroups
	viewAI
	viewHub
	viewCount
)

type model struct {
	root     string
	snap     snapshot
	selected int
	view     int
	width    int
	height   int
}

type tickMsg time.Time
type snapMsg snapshot

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Run starts the dashboard against the given state root.
func Run(root string) error {
	m := model{root: root}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.load(), tick())
}

func (m model) load() tea.Cmd {
	root, sel := m.root, m.selected
	return func() tea.Msg { return snapMsg(collect(root, sel)) }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(m.load(), tick())
	case snapMsg:
		m.snap = snapshot(msg)
		if m.selected >= len(m.snap.Projects) {
			m.selected = 0
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "j", "down":
			if m.selected < len(m.snap.Projects)-1 {
				m.selected++
				return m, m.load()
			}
		case "k", "up":
			if m.selected > 0 {
				m.selected--
				return m, m.load()
			}
		case "r":
			return m, m.load()
		case "t":
			curTheme = (curTheme + 1) % len(themes)
			applyTheme(themes[curTheme].t)
		case "tab", "l", "right":
			m.view = (m.view + 1) % viewCount
		case "shift+tab", "h", "left":
			m.view = (m.view + viewCount - 1) % viewCount
		case "1":
			m.view = viewProjects
		case "2":
			m.view = viewGroups
		case "3":
			m.view = viewAI
		case "4":
			m.view = viewHub
		}
	}
	return m, nil
}

// A theme names only ANSI palette indexes, so the terminal's own palette
// keeps ownership of exact hues (and light terminals stay readable).
type theme struct {
	title, dim, sel, warn, sess, num, headerFg, headerBg string
}

var themes = []struct {
	name string
	t    theme
}{
	{"green", theme{"14", "8", "11", "9", "2", "3", "0", "2"}},
	{"blue", theme{"14", "8", "11", "9", "12", "6", "15", "4"}},
	{"amber", theme{"11", "8", "15", "9", "3", "11", "0", "3"}},
	{"mono", theme{"15", "8", "15", "9", "7", "7", "0", "7"}},
}

// curTheme indexes themes; the initial pick comes from AIMEM_TUI_THEME
// (folded in from ~/.config/aimem/env like the other AIMEM_* vars) and
// the t key cycles at runtime.
var curTheme = 0

var (
	titleSt  lipgloss.Style
	dimSt    lipgloss.Style
	selSt    lipgloss.Style
	warnSt   lipgloss.Style
	sessSt   lipgloss.Style
	numSt    lipgloss.Style
	headerSt lipgloss.Style // htop-style column bar, full width
)

func applyTheme(t theme) {
	titleSt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.title))
	dimSt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.dim))
	selSt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.sel))
	warnSt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.warn))
	sessSt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.sess))
	numSt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.num))
	headerSt = lipgloss.NewStyle().
		Foreground(lipgloss.Color(t.headerFg)).Background(lipgloss.Color(t.headerBg))
}

func init() {
	if want := os.Getenv("AIMEM_TUI_THEME"); want != "" {
		for i, th := range themes {
			if th.name == want {
				curTheme = i
			}
		}
	}
	applyTheme(themes[curTheme].t)
}

func (m model) View() string {
	s := m.snap
	// Adapt to the terminal: wide columns absorb extra width, every line
	// clips to it, and the project list yields rows on short screens.
	w := m.width
	if w <= 0 {
		w = 100
	}
	compact := w < 80      // narrow terminal: drop low-value columns
	nameW := max(20, w-55) // name column absorbs all spare width
	reqW := max(20, w-36)
	if compact {
		nameW = max(16, w-25)
		reqW = max(16, w-14)
	}
	maxRows := len(s.Projects)
	if m.height > 0 {
		// Fixed chrome: header, detail block, groups, footer (~15 lines).
		if avail := m.height - 15 - len(s.Tail) - len(s.Groups); avail < maxRows {
			maxRows = max(3, avail)
		}
	}
	var b strings.Builder
	help := "refreshed " + s.When.Format("15:04:05")
	if !compact {
		help += "   tab/1-4 view · j/k select · r refresh · t theme · q quit"
	}
	fmt.Fprintf(&b, "%s  %s  %s\n", titleSt.Render("aimem"), m.tabBar(), dimSt.Render(help))
	if s.Err != nil {
		fmt.Fprintf(&b, "%s\n", warnSt.Render("error: "+s.Err.Error()))
	}
	switch m.view {
	case viewGroups:
		m.renderGroups(&b, w)
	case viewAI:
		m.renderAI(&b, w)
	case viewHub:
		m.renderHub(&b, w)
	default:
		m.renderProjects(&b, w, nameW, reqW, maxRows, compact)
	}
	m.renderFooter(&b)
	return b.String()
}

func (m model) tabBar() string {
	names := []string{"1:Projects", "2:Groups", "3:AI", "4:Hub"}
	parts := make([]string, len(names))
	for i, n := range names {
		if i == m.view {
			parts[i] = headerSt.Render(" " + n + " ")
		} else {
			parts[i] = dimSt.Render(" " + n + " ")
		}
	}
	return strings.Join(parts, "")
}

func (m model) renderProjects(b *strings.Builder, w, nameW, reqW, maxRows int, compact bool) {
	s := m.snap
	// Pending doc-merge conflicts outrank everything: they are the one
	// state on this machine that waits for a human.
	for _, c := range s.Conflicts {
		fmt.Fprintf(b, "  %s\n", warnSt.Render(clip("merge pending — "+c, max(30, w-4))))
	}
	// Projects table.
	if compact {
		fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %5s %-9s",
			nameW, "project", "mems", "last")))
	} else {
		fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %6s %5s %5s %6s %-8s %-11s %-13s",
			nameW, "project", "events", "sess", "mems", "embed", "hub", "client", "last activity")))
	}
	start := 0
	if m.selected >= maxRows {
		start = m.selected - maxRows + 1 // keep the selection visible
	}
	if start > 0 {
		fmt.Fprintf(b, "  %s\n", dimSt.Render(fmt.Sprintf("… %d above", start)))
	}
	for i := start; i < len(s.Projects) && i < start+maxRows; i++ {
		p := s.Projects[i]
		var line string
		if compact {
			line = fmt.Sprintf("%-*s %5d %-9s",
				nameW, clip(p.ID, nameW), p.Stats.Memories, ago(p.Stats.LastEventTS))
		} else {
			line = fmt.Sprintf("%-*s %6d %5d %5d %5d%% %-8s %-11s %-13s",
				nameW, clip(p.ID, nameW), p.Stats.Events, p.Stats.Sessions, p.Stats.Memories,
				pct(p.Stats.Embedded, p.Stats.Memories), clip(cmp.Or(p.Hub, "-"), 8),
				clip(p.Stats.LastClient, 11), ago(p.Stats.LastEventTS))
		}
		if i == m.selected {
			line = selSt.Render("> " + line)
		} else {
			line = "  " + line
		}
		fmt.Fprintln(b, line)
	}
	if rest := len(s.Projects) - (start + maxRows); rest > 0 {
		fmt.Fprintf(b, "  %s\n", dimSt.Render(fmt.Sprintf("… %d more", rest)))
	}

	// Selected project detail: groups, last curation, live tail.
	if len(s.Projects) > 0 {
		p := s.Projects[m.selected]
		fmt.Fprintf(b, "\n%s %s\n", titleSt.Render("selected:"), p.ID)
		if len(p.Groups) > 0 {
			fmt.Fprintf(b, "  groups: %s\n", strings.Join(p.Groups, ", "))
		}
		if c := p.Curate; c != nil {
			fmt.Fprintf(b, "  last curation: %s  read %d -> wrote %d (re %d, skip %d)  tokens %d in / %d out\n",
				ago(c.TS), c.EventsRead, c.Written, c.Reasserted, c.Skipped,
				c.InputTokens, c.OutputTokens)
		} else {
			fmt.Fprintf(b, "  last curation: %s\n", dimSt.Render("never"))
		}
		for _, e := range s.Tail {
			req := e.UserRequest
			if req == "" {
				req = dimSt.Render("(no request)")
			}
			if compact {
				fmt.Fprintf(b, "  %s %s\n", dimSt.Render(ago(e.TS)), clip(req, reqW))
				continue
			}
			sid := e.SessionID
			if len(sid) > 12 {
				sid = sid[:12]
			}
			fmt.Fprintf(b, "  %s %s %-24s %s\n", dimSt.Render(ago(e.TS)),
				e.Kind+"/"+e.Outcome, dimSt.Render(e.Client+":"+sid), clip(req, max(20, reqW-8)))
		}
	}
}

// renderGroups: shared knowledge scopes — who declares them, what they hold.
func (m model) renderGroups(b *strings.Builder, w int) {
	s := m.snap
	compact := w < 80
	gw := max(24, min(40, w-60))
	if compact {
		gw = max(16, w-18)
	}
	fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %5s %6s  %s", gw, "group", "facts", "embed", "declared by")))
	byID := map[string]projectRow{}
	for _, p := range s.Projects {
		byID[p.ID] = p
	}
	ids := make([]string, 0, len(s.Groups))
	for id := range s.Groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Group DBs exist as projects too; include ones nobody currently declares.
	for _, p := range s.Projects {
		if strings.HasPrefix(p.ID, "group-") && !slices.Contains(ids, p.ID) {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		fmt.Fprintf(b, "  %s\n", dimSt.Render("no groups declared (add {\"groups\":[...]} to a project's .aimem.json)"))
	}
	for _, id := range ids {
		st := byID[id].Stats
		decl := strings.Join(s.Groups[id], ", ")
		if decl == "" {
			decl = dimSt.Render("(no active declarers)")
		}
		pol := ""
		if p := s.GroupPolicy[id]; p != "" && p != "domain" {
			pol = "  " + numSt.Render("policy:"+p)
		}
		if compact {
			fmt.Fprintf(b, "  %-*s %5d\n      %s%s\n", gw, clip(id, gw), st.Memories,
				dimSt.Render(clip(decl, max(16, w-8))), pol)
		} else {
			embedded := fmt.Sprintf("%5d%%", pct(st.Embedded, st.Memories))
			if st.Memories == 0 {
				embedded = "    " + dimSt.Render("-") + " "
			}
			fmt.Fprintf(b, "  %-*s %5d %s  %s%s\n", gw, clip(id, gw), st.Memories,
				embedded, clip(decl, max(20, w-gw-28)), pol)
		}
		if about := s.GroupAbout[id]; about != "" {
			fmt.Fprintf(b, "      %s %s\n", dimSt.Render("≡"),
				sessSt.Render(clip(about, max(20, w-10))))
		}
		if chaps := s.GroupChaps[id]; len(chaps) > 0 {
			parts := make([]string, 0, len(chaps))
			for _, c := range chaps {
				parts = append(parts, fmt.Sprintf("%s %s", c.Name, numSt.Render(strconv.Itoa(c.Count))))
			}
			fmt.Fprintf(b, "      %s %s\n", dimSt.Render("§"), strings.Join(parts, dimSt.Render(" · ")))
		}
		for _, txt := range s.GroupFacts[id] {
			fmt.Fprintf(b, "      %s %s\n", dimSt.Render("·"), clip(txt, max(20, w-10)))
		}
		// Pad AFTER clipping each field: clip() collapses space runs, so
		// running an aligned line through it would destroy the columns.
		// Style per field so the row isn't one flat dim color.
		pw := max(12, w-56)
		for _, gs := range s.GroupSess[id] {
			sid := gs.SessionID
			if len(sid) > 12 {
				sid = sid[:12]
			}
			fmt.Fprintf(b, "      %s %s  %s %-*s %s\n", dimSt.Render("⎇"),
				dimSt.Render(fmt.Sprintf("%8s", ago(gs.LastTS))),
				sessSt.Render(fmt.Sprintf("%-25s", clip(gs.Client+":"+sid, 25))),
				pw, clip(gs.Project, pw),
				numSt.Render(fmt.Sprintf("%5d ev", gs.Events)))
		}
	}
}

// renderAI: model/token side — curation cost per project and per group
// over time (today / 7 days / 30 days, tokens in+out).
func (m model) renderAI(b *strings.Builder, w int) {
	s := m.snap
	fmt.Fprintf(b, "\n  curate model: %s   embed model: %s\n",
		orDim(s.CurateModel, "not configured"), orDim(s.EmbedModel, "not configured"))
	compact := w < 80
	aw := max(30, w-40) // wide column absorbs spare width
	if compact {
		aw = max(16, w-22)
		fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %8s %8s", aw, "project", "today", "30d")))
	} else {
		fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %-9s %8s %8s %8s",
			aw, "project", "last run", "today", "7d", "30d")))
	}
	var today, week, month usage
	shown := 0
	for _, p := range s.Projects {
		if p.Curate == nil && p.Month.total() == 0 {
			continue
		}
		shown++
		today.add(p.Today)
		week.add(p.Week)
		month.add(p.Month)
		last := "-"
		if p.Curate != nil {
			last = ago(p.Curate.TS)
		}
		if compact {
			fmt.Fprintf(b, "  %-*s %8s %8s\n", aw, clip(p.ID, aw),
				tok(p.Today.total()), tok(p.Month.total()))
		} else {
			fmt.Fprintf(b, "  %-*s %-9s %8s %8s %8s\n", aw, clip(p.ID, aw), last,
				tok(p.Today.total()), tok(p.Week.total()), tok(p.Month.total()))
		}
	}
	if shown == 0 {
		fmt.Fprintf(b, "  %s\n", dimSt.Render("no curation runs recorded yet (nightly hub timer, or run `aimem curate`)"))
	} else {
		if compact {
			fmt.Fprintf(b, "  %s %8s %8s\n",
				selSt.Render(fmt.Sprintf("%-*s", aw, "total")),
				tok(today.total()), tok(month.total()))
		} else {
			fmt.Fprintf(b, "  %s %8s %8s %8s\n",
				selSt.Render(fmt.Sprintf("%-*s", aw+10, "total (in+out tokens)")),
				tok(today.total()), tok(week.total()), tok(month.total()))
		}
	}

	// Per-group rollup: a group's cost is the sum over projects declaring
	// it (a multi-group project counts in each of its groups).
	if len(s.Groups) > 0 && shown > 0 {
		byID := map[string]projectRow{}
		for _, p := range s.Projects {
			byID[p.ID] = p
		}
		ids := make([]string, 0, len(s.Groups))
		for id := range s.Groups {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if compact {
			fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %8s %8s", aw, "group", "today", "30d")))
		} else {
			fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %-9s %8s %8s %8s",
				aw, "group (sum of member projects)", "", "today", "7d", "30d")))
		}
		var grouped map[string]bool = map[string]bool{}
		for _, gid := range ids {
			var t, wk, mo usage
			for _, pid := range s.Groups[gid] {
				p := byID[pid]
				t.add(p.Today)
				wk.add(p.Week)
				mo.add(p.Month)
				grouped[pid] = true
			}
			if compact {
				fmt.Fprintf(b, "  %-*s %8s %8s\n", aw, clip(gid, aw), tok(t.total()), tok(mo.total()))
			} else {
				fmt.Fprintf(b, "  %-*s %-9s %8s %8s %8s\n", aw, clip(gid, aw), "",
					tok(t.total()), tok(wk.total()), tok(mo.total()))
			}
		}
		var t, wk, mo usage
		for _, p := range s.Projects {
			if !grouped[p.ID] {
				t.add(p.Today)
				wk.add(p.Week)
				mo.add(p.Month)
			}
		}
		if mo.total() > 0 {
			fmt.Fprintf(b, "  %-*s %-9s %8s %8s %8s\n", aw+len(dimSt.Render(""))*0+0, dimSt.Render(fmt.Sprintf("%-*s", aw, "(ungrouped projects)")), "",
				tok(t.total()), tok(wk.total()), tok(mo.total()))
		}
	}
	// Per-model breakdown (which model burned the tokens).
	if len(s.Models) > 0 {
		names := make([]string, 0, len(s.Models))
		for n := range s.Models {
			names = append(names, n)
		}
		sort.Strings(names)
		if compact {
			fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %8s %8s", aw, "model", "today", "30d")))
		} else {
			fmt.Fprintf(b, "\n%s\n", bar(w, fmt.Sprintf("  %-*s %-9s %8s %8s %8s",
				aw, "model", "", "today", "7d", "30d")))
		}
		for _, n := range names {
			mu := s.Models[n]
			if compact {
				fmt.Fprintf(b, "  %-*s %8s %8s\n", aw, clip(n, aw),
					tok(mu.Today.total()), tok(mu.Month.total()))
			} else {
				fmt.Fprintf(b, "  %-*s %-9s %8s %8s %8s\n", aw, clip(n, aw), "",
					tok(mu.Today.total()), tok(mu.Week.total()), tok(mu.Month.total()))
			}
		}
	}
	if len(s.Budget) > 0 {
		fmt.Fprintf(b, "\n%s\n", titleSt.Render("  budget (global)"))
		for _, bl := range s.Budget {
			var parts []string
			var frac float64
			mark := func(f float64) {
				if f > frac {
					frac = f
				}
			}
			if c := bl.Cap.Tokens; c > 0 {
				f := float64(bl.UsedIn+bl.UsedOut) / float64(c)
				mark(f)
				parts = append(parts, fmt.Sprintf("%s / %s tokens (%d%%)",
					tok(bl.UsedIn+bl.UsedOut), tok(c), int(100*f)))
			}
			if c := bl.Cap.TokensIn; c > 0 {
				f := float64(bl.UsedIn) / float64(c)
				mark(f)
				parts = append(parts, fmt.Sprintf("in %s / %s (%d%%)",
					tok(bl.UsedIn), tok(c), int(100*f)))
			}
			if c := bl.Cap.TokensOut; c > 0 {
				f := float64(bl.UsedOut) / float64(c)
				mark(f)
				parts = append(parts, fmt.Sprintf("out %s / %s (%d%%)",
					tok(bl.UsedOut), tok(c), int(100*f)))
			}
			if c := bl.Cap.USD; c > 0 {
				f := bl.UsedUSD / c
				mark(f)
				parts = append(parts, fmt.Sprintf("$%.4f / $%.2f (%d%%)",
					bl.UsedUSD, c, int(100*f)))
			}
			line := fmt.Sprintf("  %-8s %s", bl.Window, strings.Join(parts, " · "))
			if frac >= 0.8 {
				line = warnSt.Render(line)
			}
			fmt.Fprintln(b, line)
		}
	}
	fmt.Fprintf(b, "\n  %s\n", dimSt.Render("curation + embedding spend metered; LiteLLM per-key spend is the authoritative ledger"))
}

// tok renders a token count compactly (11.1k).
func tok(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n < 10_000:
		return fmt.Sprintf("%d", n)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}

func (m model) renderFooter(b *strings.Builder) {
	s := m.snap
	if m.view == viewHub {
		return // the Hub tab already shows all of this
	}
	fmt.Fprintf(b, "\n%s ", titleSt.Render("hub:"))
	if s.Hub == "" {
		b.WriteString(dimSt.Render("not configured"))
	} else {
		b.WriteString(s.Hub)
	}
	if r := s.HubRes; r != nil {
		mem := fmt.Sprintf("mem %v%% (%vMB free)", r["mem_used_pct"], r["mem_available_mb"])
		disk := fmt.Sprintf("disk %v%% (%vMB free)", r["disk_used_pct"], r["disk_free_mb"])
		line := fmt.Sprintf("  %s · %s · rss %vMB", mem, disk, r["proc_rss_mb"])
		if asInt(r["mem_used_pct"]) >= 85 || asInt(r["disk_used_pct"]) >= 85 {
			line = warnSt.Render(line)
		} else {
			line = dimSt.Render(line)
		}
		b.WriteString(line)
	} else if s.Hub != "" {
		b.WriteString(dimSt.Render("  (health unreachable)"))
	}
	if s.Spool > 0 {
		fmt.Fprintf(b, "  %s", warnSt.Render(fmt.Sprintf("spool: %d queued", s.Spool)))
	}
	b.WriteString("\n")
	for _, t := range s.Timers {
		fmt.Fprintf(b, "%s\n", dimSt.Render("  "+t))
	}
}

// bar renders a full-width htop-style header bar.
func bar(w int, text string) string {
	if pad := w - len(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return headerSt.Render(text)
}

func orDim(s, fallback string) string {
	if s == "" {
		return dimSt.Render(fallback)
	}
	return s
}

// collect gathers one dashboard snapshot; every part is best-effort.
func collect(root string, selected int) snapshot {
	s := snapshot{When: time.Now(), Groups: map[string][]string{},
		GroupFacts: map[string][]string{}, GroupSess: map[string][]groupSession{},
		GroupAbout: map[string]string{}, GroupPolicy: map[string]string{},
		GroupChaps: map[string][]chapterCount{}, Models: map[string]*modelUsage{}}
	s.CurateModel, s.EmbedModel = modelConfig()
	reg, err := store.NewRegistry(root)
	if err != nil {
		s.Err = err
		return s
	}
	defer reg.Close()
	ids, err := reg.Projects()
	if err != nil {
		s.Err = err
		return s
	}
	sort.Strings(ids)
	ac := adapter.NewClient(root)
	for _, id := range ids {
		db, err := reg.Open(id)
		if err != nil {
			continue
		}
		row := projectRow{ID: id}
		row.Stats, _ = db.Stats()
		row.Hub, _ = db.GetMeta("hub")
		// Pending doc-merge previews (DESIGN-doc-collab): local files, so
		// this costs no network — exactly the "this machine needs a human"
		// state an always-on dashboard exists to show.
		if dir := ac.DocDir(id); dir != "" {
			for _, rel := range ident.ProjectDocs(dir) {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)) + ".merge"); err == nil {
					s.Conflicts = append(s.Conflicts,
						fmt.Sprintf("%s: %s.merge — resolve with `aimem docs merge %s`", id, rel, ident.DocName(rel)))
				}
			}
		}
		if raw, _ := db.GetMeta("groups"); raw != "" {
			json.Unmarshal([]byte(raw), &row.Groups)
			for _, g := range row.Groups {
				s.Groups[g] = append(s.Groups[g], id)
			}
			// Sessions inside this member project, shown under the group
			// so the Groups tab answers "who feeds this scope".
			if sess, err := db.Sessions(); err == nil {
				for _, sm := range sess {
					gs := groupSession{Project: id}
					gs.SessionID, _ = sm["session_id"].(string)
					gs.Client, _ = sm["client"].(string)
					gs.LastTS, _ = sm["last_ts"].(string)
					if n, ok := sm["events"].(int); ok {
						gs.Events = n
					}
					for _, g := range row.Groups {
						s.GroupSess[g] = append(s.GroupSess[g], gs)
					}
				}
			}
		}
		now := time.Now().UTC()
		day := now.Truncate(24 * time.Hour).Format(time.RFC3339)
		week := now.AddDate(0, 0, -7).Format(time.RFC3339)
		month := now.AddDate(0, 0, -30).Format(time.RFC3339)
		if runs, err := db.CurateRuns(); err == nil {
			for _, r := range runs {
				// Last CURATION run: newest non-embedding row (embed usage
				// is recorded under the embedding model's name).
				if row.Curate == nil && r.Model != s.EmbedModel {
					rc := r
					row.Curate = &rc
				}
				if r.TS < month {
					continue
				}
				u := usage{In: r.InputTokens, Out: r.OutputTokens}
				name := r.Model
				if name == "" {
					name = "(unknown)"
				}
				mu := s.Models[name]
				if mu == nil {
					mu = &modelUsage{}
					s.Models[name] = mu
				}
				row.Month.add(u)
				mu.Month.add(u)
				if r.TS >= week {
					row.Week.add(u)
					mu.Week.add(u)
				}
				if r.TS >= day {
					row.Today.add(u)
					mu.Today.add(u)
				}
			}
		}
		if strings.HasPrefix(id, "group-") {
			s.GroupAbout[id], _ = db.GetMeta("about")
			s.GroupPolicy[id], _ = db.GetMeta("policy")
			var chaps []struct {
				Name string `json:"name"`
			}
			if raw, _ := db.GetMeta("chapters"); raw != "" {
				json.Unmarshal([]byte(raw), &chaps)
			}
			if mems, err := db.Memories(false); err == nil {
				byChap := map[string]int{}
				for _, mm := range mems {
					for _, tag := range mm.Tags {
						if n, ok := strings.CutPrefix(tag, "chapter:"); ok {
							byChap[n]++
						}
					}
				}
				for _, c := range chaps {
					s.GroupChaps[id] = append(s.GroupChaps[id], chapterCount{Name: c.Name, Count: byChap[c.Name]})
				}
				sort.Slice(mems, func(i, j int) bool { return mems[i].CreatedAt > mems[j].CreatedAt })
				for i, mm := range mems {
					if i >= 3 {
						break
					}
					s.GroupFacts[id] = append(s.GroupFacts[id], mm.Text)
				}
			}
		}
		s.Projects = append(s.Projects, row)
	}
	// Most recently active first, so the dashboard leads with live work.
	sort.SliceStable(s.Projects, func(i, j int) bool {
		return s.Projects[i].Stats.LastEventTS > s.Projects[j].Stats.LastEventTS
	})
	for g, sess := range s.GroupSess {
		sort.SliceStable(sess, func(i, j int) bool { return sess[i].LastTS > sess[j].LastTS })
		if len(sess) > 4 {
			sess = sess[:4]
		}
		s.GroupSess[g] = sess
	}
	if selected < len(s.Projects) {
		if db, err := reg.Open(s.Projects[selected].ID); err == nil {
			if evs, err := db.RecentEvents(5); err == nil {
				s.Tail = evs
			}
		}
	}
	if udb, err := reg.Open(store.UserScopeProject); err == nil {
		if raw, _ := udb.GetMeta("budget"); raw != "" {
			var bg curate.Budget
			if json.Unmarshal([]byte(raw), &bg) == nil {
				now := time.Now()
				for _, wc := range []struct {
					name string
					cap  *curate.Cap
				}{{"daily", bg.Daily}, {"weekly", bg.Weekly}, {"monthly", bg.Monthly}} {
					if wc.cap == nil {
						continue
					}
					inTok, outTok, usd, err := curate.WindowUsage(reg, udb, true, wc.name, now, bg.Epoch)
					if err != nil {
						continue
					}
					s.Budget = append(s.Budget, budgetLine{
						Window: wc.name, UsedIn: inTok, UsedOut: outTok, UsedUSD: usd, Cap: *wc.cap})
				}
			}
		}
	}
	if hubs, def := adapter.LoadHubs(root); hubs != nil {
		for _, name := range slices.Sorted(maps.Keys(hubs)) {
			h := hubs[name]
			res, projs, ok := cachedHubHealth(h.URL+"/v1/health", h.Token, h.Insecure)
			s.Hubs = append(s.Hubs, hubLine{
				Name: name, URL: h.URL, Sync: h.Sync, Default: name == def,
				Res: res, Projects: projs, OK: ok})
			if name == def {
				s.Hub, s.HubRes, s.HubProjects, s.HubOK = h.URL, res, projs, ok
			}
		}
	}
	// Spools are per destination (spool/pending.jsonl for the local
	// service, spool/hub-<name>.jsonl per hub) — count EVENTS per file,
	// not files, so "which hub is behind, and by how much" is answerable.
	// In-flight .replay-* claims are deliberately excluded.
	if entries, err := os.ReadDir(filepath.Join(root, "spool")); err == nil {
		s.SpoolBy = map[string]int{}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, "spool", name))
			if err != nil || len(raw) == 0 {
				continue
			}
			n := bytes.Count(raw, []byte{'\n'})
			if n == 0 {
				n = 1 // a partial final line is still one queued event
			}
			label := "local service"
			if h, ok := strings.CutPrefix(strings.TrimSuffix(name, ".jsonl"), "hub-"); ok {
				label = "hub " + h
			}
			s.SpoolBy[label] += n
			s.Spool += n
		}
	}
	// systemd user timers (Linux best-effort; silently absent elsewhere).
	if out, err := exec.Command("systemctl", "--user", "list-timers",
		"aimem-*", "--no-pager", "--no-legend").Output(); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				s.Timers = append(s.Timers, squeeze(l))
			}
		}
	}
	return s
}

// modelConfig reports the curation/embedding models: process env first,
// then ~/.config/aimem/env (the service's EnvironmentFile) best-effort —
// the TUI usually runs in a shell that never sourced it.
func modelConfig() (curate, embed string) {
	curate = os.Getenv("AIMEM_CURATE_MODEL")
	embed = os.Getenv("AIMEM_EMBED_MODEL")
	if curate != "" && embed != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "aimem", "env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "AIMEM_CURATE_MODEL":
			if curate == "" {
				curate = v
			}
		case "AIMEM_EMBED_MODEL":
			if embed == "" {
				embed = v
			}
		}
	}
	return
}

func pct(n, of int) int {
	if of == 0 {
		return 0
	}
	return 100 * n / of
}

func clip(s string, n int) string {
	// Rows must stay single-line: collapse any whitespace runs (requests
	// can contain newlines) before clipping.
	s = squeeze(s)
	r := []rune(s) // rune-safe: requests may contain non-ASCII text
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func ago(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if t, err = time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
			return ts
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func squeeze(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fetchHubHealth pulls the hub health endpoint; the dashboard must never
// block on the hub, so failures degrade to unreachable.
var tuiHubHTTP = &http.Client{Timeout: 1500 * time.Millisecond}
var tuiHubHTTPInsecure = &http.Client{
	Timeout:   1500 * time.Millisecond,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

func fetchHubHealth(url, token string, insecure bool) (map[string]any, int, bool) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	cl := tuiHubHTTP
	if insecure {
		cl = tuiHubHTTPInsecure
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, false
	}
	defer resp.Body.Close()
	var body struct {
		Projects  int            `json:"projects"`
		Resources map[string]any `json:"resources"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil, 0, false
	}
	return body.Resources, body.Projects, true
}

// hubLine is one configured hub's health snapshot for the Hub tab.
type hubLine struct {
	Name     string
	URL      string
	Sync     string
	Default  bool
	Res      map[string]any
	Projects int
	OK       bool
}

// renderHub: every configured hub's state — reachability, resources —
// plus local sync/spool posture and timers.
func (m model) renderHub(b *strings.Builder, w int) {
	s := m.snap
	if len(s.Hubs) == 0 {
		fmt.Fprintf(b, "\n%s\n", bar(w, "  hub"))
		fmt.Fprintf(b, "  %s\n", dimSt.Render("not configured (aimem hub <url> <token>)"))
		return
	}
	for _, h := range s.Hubs {
		title := "  hub: " + h.Name
		if h.Default {
			title += " (default)"
		}
		fmt.Fprintf(b, "\n%s\n", bar(w, title))
		m.renderHubOne(b, h)
	}
	if s.Spool > 0 {
		// Per-destination breakdown: one aggregate number cannot say
		// WHICH hub is behind on a multi-hub machine.
		var parts []string
		for _, label := range slices.Sorted(maps.Keys(s.SpoolBy)) {
			parts = append(parts, fmt.Sprintf("%s: %d", label, s.SpoolBy[label]))
		}
		fmt.Fprintf(b, "  %s\n", warnSt.Render(fmt.Sprintf("spool:      %d event(s) queued (%s)",
			s.Spool, strings.Join(parts, ", "))))
	} else {
		fmt.Fprintf(b, "  spool:      %s\n", dimSt.Render("empty (real-time push healthy)"))
	}
	if len(s.Timers) > 0 {
		fmt.Fprintf(b, "\n%s\n", bar(w, "  local timers (sync/curate)"))
		for _, t := range s.Timers {
			fmt.Fprintf(b, "  %s\n", dimSt.Render(clip(t, max(30, w-4))))
		}
	}
}

func (m model) renderHubOne(b *strings.Builder, h hubLine) {
	fmt.Fprintf(b, "  url:        %s\n", h.URL)
	if h.Sync != "" {
		fmt.Fprintf(b, "  sync:       %s\n", h.Sync)
	}
	if !h.OK {
		fmt.Fprintf(b, "  status:     %s\n", warnSt.Render("UNREACHABLE (checkpoints spool locally; sync catches up)"))
	} else {
		fmt.Fprintf(b, "  status:     ok (%d projects)\n", h.Projects)
	}
	if r := h.Res; r != nil {
		memPct, diskPct := asInt(r["mem_used_pct"]), asInt(r["disk_used_pct"])
		mem := fmt.Sprintf("  memory:     %s  (%vMB free of %vMB, aimem rss %vMB)",
			gauge(memPct, 30), r["mem_available_mb"], r["mem_total_mb"], r["proc_rss_mb"])
		disk := fmt.Sprintf("  disk:       %s  (%vMB free of %vMB)",
			gauge(diskPct, 30), r["disk_free_mb"], r["disk_total_mb"])
		if memPct >= 85 {
			mem = warnSt.Render(mem)
		}
		if diskPct >= 85 {
			disk = warnSt.Render(disk)
		}
		fmt.Fprintln(b, mem)
		fmt.Fprintln(b, disk)
		if _, ok := r["cpu_used_pct"]; ok {
			cpuPct := asInt(r["cpu_used_pct"])
			cpu := fmt.Sprintf("  cpu:        %s  (time actually busy since last poll)", gauge(cpuPct, 30))
			if cpuPct >= 90 {
				cpu = warnSt.Render(cpu)
			}
			fmt.Fprintln(b, cpu)
		}
		if l1, ok := r["load_1m"]; ok {
			load := fmt.Sprintf("  load:       %v / %v / %v (1/5/15m, %v cpus)",
				l1, r["load_5m"], r["load_15m"], r["cpus"])
			// Warn on SUSTAINED saturation (5m >= cpus): 1m load spikes on
			// a 1-vCPU box on any brief I/O wait and would cry wolf.
			if lf, err := strconv.ParseFloat(fmt.Sprint(r["load_5m"]), 64); err == nil && lf >= float64(asInt(r["cpus"])) {
				load = warnSt.Render(load)
			}
			fmt.Fprintln(b, load)
		}
		fmt.Fprintf(b, "  %s\n", dimSt.Render("service memory capped: MemoryHigh=256M MemoryMax=512M (systemd)"))
	}
}

// gauge renders a [#####-----] NN%% bar.
func gauge(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	fill := pct * width / 100
	return fmt.Sprintf("[%s%s] %d%%", strings.Repeat("#", fill),
		strings.Repeat("-", width-fill), pct)
}

func asInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// cachedHubHealth throttles hub polling to every 10s per hub: each poll
// is a full TLS handshake, and at the dashboard's 2s refresh that alone
// nudges a 1-vCPU hub's load average.
type hubHealthEntry struct {
	at    time.Time
	res   map[string]any
	projs int
	ok    bool
}

var hubHealthCache struct {
	mu sync.Mutex
	by map[string]*hubHealthEntry // keyed by health URL
}

func cachedHubHealth(url, token string, insecure bool) (map[string]any, int, bool) {
	hubHealthCache.mu.Lock()
	defer hubHealthCache.mu.Unlock()
	if hubHealthCache.by == nil {
		hubHealthCache.by = map[string]*hubHealthEntry{}
	}
	e := hubHealthCache.by[url]
	if e != nil && time.Since(e.at) < 10*time.Second {
		return e.res, e.projs, e.ok
	}
	e = &hubHealthEntry{at: time.Now()}
	e.res, e.projs, e.ok = fetchHubHealth(url, token, insecure)
	hubHealthCache.by[url] = e
	return e.res, e.projs, e.ok
}
