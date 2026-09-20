package ingest

import (
	"regexp"
	"strings"

	"dboss/internal/logstore"
)

// One row never grows past these; the line that would overflow starts a new row.
const (
	maxBlockLines = 1000
	maxBlockBytes = 256 * 1024
)

var (
	ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// Rails log_tags = [:request_id] puts "[<id>] " in front of every line.
	tagPattern     = regexp.MustCompile(`^\[([\w\-@]{8,64})\] ?`)
	railsStarted   = regexp.MustCompile(`^Started (GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) "`)
	railsCompleted = regexp.MustCompile(`^Completed (\d)\d\d `)
)

func stripANSI(line string) string {
	if !strings.Contains(line, "\x1b") {
		return line
	}
	return ansiPattern.ReplaceAllString(line, "")
}

// splitTag cuts a leading request id tag off the line. The id needs a digit, so word tags such
// as [ActiveJob] stay part of the text.
func splitTag(text string) (string, string) {
	match := tagPattern.FindStringSubmatch(text)
	if match == nil || !strings.ContainsAny(match[1], "0123456789") {
		return "", text
	}
	return match[1], text[len(match[0]):]
}

// grouper folds the physical lines of one log into rows. A line that starts with whitespace
// continues the row above it, and a Rails "Started ..." line keeps its row open until the
// matching "Completed ...". Lines only join a row that carries the same request id, so
// interleaved requests split into more rows instead of mixing.
type grouper struct {
	source, name string
	entries      []logstore.LogEntry

	raw    []string // open row, lines as written
	text   []string // open row, without colors and request tag
	size   int
	id     string
	rails  bool
	level  string
	blanks int // blank lines seen since the last line of the open row
}

func (g *grouper) open() bool { return len(g.raw) > 0 }

// tail returns the lines of the open row as written.
func (g *grouper) tail() []string { return g.raw }

// add takes the next line and reports whether it opened a new row.
func (g *grouper) add(line string) bool {
	id, text := splitTag(stripANSI(line))
	if strings.TrimSpace(text) == "" {
		if g.open() {
			g.blanks++
		}
		return false
	}
	if g.open() && g.joins(id, text, len(line)) {
		g.push(line, text)
		if g.rails {
			g.completed(text)
		}
		return false
	}
	g.flush()
	g.id, g.rails = id, railsStarted.MatchString(text)
	g.push(line, text)
	return true
}

func (g *grouper) joins(id, text string, size int) bool {
	if id != g.id || len(g.raw) >= maxBlockLines || g.size+size > maxBlockBytes {
		return false
	}
	if g.rails {
		return !railsStarted.MatchString(text)
	}
	return text[0] == ' ' || text[0] == '\t'
}

func (g *grouper) push(line, text string) {
	// Blank lines survive only between two lines of the same row.
	for ; g.blanks > 0; g.blanks-- {
		g.raw, g.text = append(g.raw, ""), append(g.text, "")
	}
	g.raw, g.text = append(g.raw, line), append(g.text, text)
	g.size += len(line)
}

// completed closes a Rails request row on its "Completed <status>" line and grades the row by
// the status class.
func (g *grouper) completed(text string) {
	match := railsCompleted.FindStringSubmatch(text)
	if match == nil {
		return
	}
	switch match[1] {
	case "5":
		g.level = "error"
	case "4":
		g.level = "warn"
	}
	g.flush()
}

// flush emits the open row. Level and JSON fields come from its first line only, so detail lines
// such as SQL text cannot change the level.
func (g *grouper) flush() {
	if !g.open() {
		return
	}
	entry := ParseLine(g.source, g.name, g.raw[0])
	if len(g.raw) > 1 {
		entry.Message += "\n" + strings.Join(g.text[1:], "\n")
		entry.Raw = strings.Join(g.raw, "\n")
	}
	if g.level != "" {
		entry.Level = g.level
	}
	g.entries = append(g.entries, entry)
	g.raw, g.text, g.size, g.id, g.rails, g.level, g.blanks = nil, nil, 0, "", false, "", 0
}
