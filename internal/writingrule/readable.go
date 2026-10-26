package writingrule

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Finding is the first place a text breaks the rule, in the terms a
// refusal hands back to the writer: the exact span, why it was flagged,
// the rule's section and the one-sentence fix.
type Finding struct {
	Span    string `json:"span"`
	Reason  string `json:"reason"`
	Section string `json:"section"`
	Fix     string `json:"fix"`
}

// The reasons a text is flagged.
const (
	ReasonGlued       = "words and numbers joined together"
	ReasonSpacing     = "too few spaces between words"
	ReasonPunctuation = "no space after sentence punctuation"
)

// RuleSection names where the writer reads what the check enforces.
const RuleSection = `"The rule" in ` + Source + " (the writing_rule tool returns it)"

var fixes = map[string]string{
	ReasonGlued:       `Separate the words from the numbers and write the sentence out, for example "run 409 succeeded in 99 seconds" instead of "run409 SUCCESS99s".`,
	ReasonSpacing:     "Write complete sentences with a space between every two words.",
	ReasonPunctuation: "Put a space after the punctuation that ends each sentence, and write each sentence out in full.",
}

// Exact text is exempt: the rule keeps code, commands, identifiers, paths,
// hashes, versions and URLs as they are, so they are cut out before the
// heuristics run. Order matters: fences before inline code, URLs before
// paths.
var exempt = []*regexp.Regexp{
	regexp.MustCompile("(?s)```.*?(```|$)"),
	regexp.MustCompile("(?s)~~~.*?(~~~|$)"),
	regexp.MustCompile("`[^`\n]*`"),
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`),
	regexp.MustCompile(`\S*[/\\]\S*`),                      // paths, and slash pairs such as and/or
	regexp.MustCompile(`\b[0-9A-Fa-f]+(-[0-9A-Fa-f]+)+\b`), // UUIDs and other dashed hex ids
	regexp.MustCompile(`\b[0-9A-Fa-f]{7,}\b`),              // hashes
	regexp.MustCompile(`\b[vV]?\d+(\.\d+)+[\w.+-]*`),       // versions
	regexp.MustCompile(`\S*[_@=<>{}\[\]|$%^~]\S*`),         // identifiers, flags' values, markup
	regexp.MustCompile(`(^|\s)--?[A-Za-z][\w-]*`),          // command-line flags
	regexp.MustCompile(`\b\w+\.[a-z]{1,5}\b`),              // file names and qualified names
}

// A letter run glued to digits is a technical name for these prefixes
// (sha256, RFC9110, win32api), not compressed prose.
var technicalPrefix = map[string]bool{
	"sha": true, "md": true, "crc": true, "ed": true, "x": true, "p": true,
	"rfc": true, "iso": true, "cve": true, "utf": true, "ucs": true,
	"int": true, "uint": true, "float": true, "complex": true, "base": true,
	"ipv": true, "http": true, "h": true, "win": true, "amd": true, "arm": true,
	"aes": true, "rsa": true, "ecdsa": true, "secp": true, "ieee": true,
	"poly": true, "curve": true, "seq": true,
}

var (
	word = regexp.MustCompile(`[A-Za-z0-9]+`)
	// letters, two or more digits, letters: Checkpoint74files, SUCCESS99s.
	gluedInside = regexp.MustCompile(`^([A-Za-z]{2,})\d{2,}[A-Za-z]`)
	// a word ending in three or more digits: approval573, CI409.
	gluedEnd = regexp.MustCompile(`^([A-Za-z]{2,})\d{3,}$`)
	// a capitalised word run into a lowercase tail and a number: BLOCKEDr4,
	// but not a version suffix such as UUIDv7.
	gluedCaps = regexp.MustCompile(`^[A-Z]{3,}([a-z]+)\d`)

	sentenceGlued  = regexp.MustCompile(`[a-z0-9)][.!?;][A-Z]|[a-z][,;+][a-z]`)
	sentenceSpaced = regexp.MustCompile(`[.!?;,]\s`)
)

const (
	// minSpacingRun is how much exempt-free text the spacing heuristic
	// needs before it judges; a short label is not prose.
	minSpacingRun = 40
	// minGluedRun is the longest run without a space the spacing
	// heuristic needs before it flags: English words stop near 22
	// letters, so a short title of long words is never a glued run.
	minGluedRun = 25
	// minSpaceShare is the share of whitespace below which text is not
	// prose; English prose runs at about one character in six.
	minSpaceShare = 0.08
	// minGluedBreaks is how many glued sentence breaks it takes, and they
	// must outnumber the spaced ones.
	minGluedBreaks = 3
)

// Unreadable reports the first span of text that breaks the rule's
// spacing and joining requirements, or nil when the text reads as prose.
// It is a filter, not a judge: it never flags exact text (code, paths,
// ids, hashes, versions, URLs), and a flagged text may still be
// readable, which is what the hub's optional second stage asks a model.
func Unreadable(text string) *Finding {
	prose := text
	for _, re := range exempt {
		prose = re.ReplaceAllString(prose, " ")
	}
	for _, w := range word.FindAllString(prose, -1) {
		if gluedWord(w) {
			return finding(w, ReasonGlued)
		}
	}
	if span, ok := sparse(prose); ok {
		return finding(span, ReasonSpacing)
	}
	if glued := sentenceGlued.FindAllStringIndex(prose, -1); len(glued) >= minGluedBreaks &&
		len(glued) > len(sentenceSpaced.FindAllStringIndex(prose, -1)) {
		return finding(fieldAt(prose, glued[0][0]), ReasonPunctuation)
	}
	return nil
}

func gluedWord(w string) bool {
	for _, re := range []*regexp.Regexp{gluedInside, gluedEnd} {
		if m := re.FindStringSubmatch(w); m != nil {
			return !technicalPrefix[strings.ToLower(m[1])]
		}
	}
	m := gluedCaps.FindStringSubmatch(w)
	return m != nil && m[1] != "v"
}

// sparse reports text whose words are not separated: a run without a
// space longer than any word, in a stretch long enough to be prose with
// little whitespace across it. The span is that longest run.
func sparse(prose string) (string, bool) {
	fields := strings.Fields(prose)
	letters := 0
	longest := ""
	for _, f := range fields {
		letters += utf8.RuneCountInString(f)
		if utf8.RuneCountInString(f) > utf8.RuneCountInString(longest) {
			longest = f
		}
	}
	if letters < minSpacingRun || utf8.RuneCountInString(longest) < minGluedRun {
		return "", false
	}
	// Only the gaps between the remaining words count: the exempt spans
	// were blanked, and their blanks are not the writer's spacing.
	if float64(len(fields)-1)/float64(letters+len(fields)-1) >= minSpaceShare {
		return "", false
	}
	return longest, true
}

// fieldAt is the whitespace-delimited field around byte offset i.
func fieldAt(s string, i int) string {
	start := strings.LastIndexAny(s[:i], " \t\n") + 1
	end := len(s)
	if j := strings.IndexAny(s[i:], " \t\n"); j >= 0 {
		end = i + j
	}
	return s[start:end]
}

func finding(span, reason string) *Finding {
	return &Finding{Span: span, Reason: reason, Section: RuleSection, Fix: fixes[reason]}
}

// Context is the stretch of text around the first occurrence of span,
// at most max bytes on each side and cut on rune boundaries, so a
// second-stage question carries bounded context.
func Context(text, span string, max int) string {
	i := strings.Index(text, span)
	if i < 0 {
		i = 0
	}
	start, end := i-max, i+len(span)+max
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return text[start:end]
}

// Message says what the finding is and where the rule lives, the same on
// every surface (hub refusal, MCP result, report); the fix goes with it
// as the next action.
func (f *Finding) Message() string {
	return "the text is not readable prose (" + f.Reason + ` at "` + f.Span + `"). The rule: ` + f.Section + "."
}
