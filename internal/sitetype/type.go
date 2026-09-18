// Package sitetype labels a public site with one of a closed set of types so
// the showcase can be browsed by kind.
//
// Nothing in this package is on the deploy path. It reads text the search
// indexer has already extracted into Postgres and never touches site storage,
// per-site locks, or version leases.
package sitetype

import "strings"

// Type is one label from the closed set. The empty Type means "not classified",
// which is a normal permanent state.
type Type string

const (
	Docs      Type = "docs"
	Dashboard Type = "dashboard"
	Deck      Type = "deck"
	Tool      Type = "tool"
	Plan      Type = "plan"
	Landing   Type = "landing"
	Starter   Type = "starter"
)

// All is the closed set, in the order the showcase presents it: the kinds
// somebody is most likely to be looking for first.
var All = []struct {
	Type  Type
	Label string
}{
	{Docs, "Docs"},
	{Dashboard, "Dashboards"},
	{Deck, "Decks"},
	{Tool, "Tools"},
	{Plan, "Plans"},
	{Landing, "Landing"},
	{Starter, "Starters"},
}

// Label returns the display name, or "" for an unknown value. Unknown is
// possible: a type read back from a database written by a newer binary.
func (t Type) Label() string {
	for _, known := range All {
		if known.Type == t {
			return known.Label
		}
	}
	return ""
}

// Valid reports whether t is a member of the closed set.
func (t Type) Valid() bool { return t.Label() != "" }

// Parse maps a model reply or a query parameter onto the set, returning ""
// for anything unrecognised. It is deliberately strict: a label outside the
// set would create a group of one and defeat the point of grouping.
func Parse(raw string) Type {
	candidate := Type(strings.ToLower(strings.TrimSpace(raw)))
	if candidate.Valid() {
		return candidate
	}
	return ""
}

// Prompt is the system prompt. It describes each type by what a visitor DOES
// with the page, because that is the distinction that stays stable — a gantt
// chart appears in both a plan and a tool that draws plans.
const Prompt = `You label a hosted static website with exactly one type.

Reply with ONLY the type id, nothing else. Choose from:
docs      - reference material: specs, API docs, architecture, schemas, onboarding guides
dashboard - shows current numbers or status somebody checks repeatedly; trackers, consoles
deck      - a slide presentation meant to be walked through
tool      - something the visitor operates to produce a result: generators, calculators, games, editors
plan      - work laid out over time: gantt charts, roadmaps, timelines, delivery plans
landing   - markets or introduces a product or team; the page is the pitch
starter   - placeholder, demo, template or throwaway test with no real content yet

If two fit, pick the one describing what the visitor DOES with the page.
Reply with one word from that list.`
