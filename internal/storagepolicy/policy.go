// Package storagepolicy is the access model of site storage
// (docs/designs/storage-presets.md): four actions, five "who" values, the
// four rules a matrix must pass, the seven presets, and table columns. It
// holds no state and touches no database, so the rules are tested alone and
// the handler only asks it questions.
package storagepolicy

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Who is one of the five values an action can take, narrowest first.
type Who string

const (
	Nobody   Who = "nobody"
	Owner    Who = "owner"
	Own      Who = "own"
	SignedIn Who = "signed-in"
	Anyone   Who = "anyone"
)

// Values lists the five values, narrowest first.
var Values = []Who{Nobody, Owner, Own, SignedIn, Anyone}

func rank(w Who) int {
	for i, v := range Values {
		if v == w {
			return i
		}
	}
	return -1
}

// Valid reports whether w is one of the five values.
func (w Who) Valid() bool { return rank(w) >= 0 }

// Action is read, add, edit or delete.
type Action string

const (
	Read   Action = "read"
	Add    Action = "add"
	Edit   Action = "edit"
	Delete Action = "delete"
)

// Actions lists the four actions in their usual order.
var Actions = []Action{Read, Add, Edit, Delete}

// Matrix gives each action its value.
type Matrix struct {
	Read   Who `json:"read"`
	Add    Who `json:"add"`
	Edit   Who `json:"edit"`
	Delete Who `json:"delete"`
}

// Get returns the value of one action.
func (m Matrix) Get(a Action) Who {
	switch a {
	case Read:
		return m.Read
	case Add:
		return m.Add
	case Edit:
		return m.Edit
	case Delete:
		return m.Delete
	}
	return Nobody
}

func (m *Matrix) set(a Action, w Who) {
	switch a {
	case Read:
		m.Read = w
	case Add:
		m.Add = w
	case Edit:
		m.Edit = w
	case Delete:
		m.Delete = w
	}
}

// UsesOwn reports whether any action is own.
func (m Matrix) UsesOwn() bool {
	return m.Read == Own || m.Add == Own || m.Edit == Own || m.Delete == Own
}

// RuleError is a matrix refused by one of the four rules.
type RuleError struct {
	Code string
	Hint string
}

func (e *RuleError) Error() string { return e.Code + ": " + e.Hint }

// Check applies the four rules, in the order the design lists them, and
// returns the first that refuses m (nil when m is allowed).
func Check(m Matrix) *RuleError {
	for _, a := range Actions {
		if !m.Get(a).Valid() {
			return &RuleError{Code: "invalid_value", Hint: fmt.Sprintf("%s must be one of nobody, owner, own, signed-in or anyone", a)}
		}
	}
	if m.Edit == Anyone || m.Delete == Anyone {
		return &RuleError{Code: "anonymous_change", Hint: "edit and delete cannot be anyone; use the inbox or wall preset for open adds"}
	}
	if m.Add == Own {
		return &RuleError{Code: "add_own", Hint: "nothing is owned before it exists; use add signed-in, and the server records who added each entry"}
	}
	if m.Add == Anyone && m.UsesOwn() {
		return &RuleError{Code: "own_needs_signed_in_add", Hint: "own needs to know who added each entry, so add must be signed-in or narrower"}
	}
	if wider(m.Edit, m.Read) || wider(m.Delete, m.Read) {
		return &RuleError{Code: "blind_change", Hint: "edit and delete cannot be wider than read: nobody changes what they cannot see"}
	}
	return nil
}

// wider reports whether a lets in somebody b does not. nobody is narrower
// than everything; own is narrower than signed-in and wider than owner.
func wider(a, b Who) bool { return rank(a) > rank(b) }

// Presets are the seven named matrices.
var Presets = map[string]Matrix{
	"public":   {Read: Anyone, Add: Owner, Edit: Owner, Delete: Owner},
	"inbox":    {Read: Owner, Add: Anyone, Edit: Owner, Delete: Owner},
	"wall":     {Read: Anyone, Add: SignedIn, Edit: Owner, Delete: Own},
	"records":  {Read: Own, Add: SignedIn, Edit: Owner, Delete: Owner},
	"personal": {Read: Own, Add: SignedIn, Edit: Own, Delete: Own},
	"board":    {Read: SignedIn, Add: SignedIn, Edit: SignedIn, Delete: Owner},
	"private":  {Read: Owner, Add: Owner, Edit: Owner, Delete: Owner},
}

// PresetOrder is the order presets are listed in.
var PresetOrder = []string{"public", "inbox", "wall", "records", "personal", "board", "private"}

// DefaultPreset is what a resource gets when nothing is chosen.
const DefaultPreset = "private"

// Spec is how an owner states a matrix: a preset, single-action overrides,
// or both. An empty spec is the default preset.
type Spec struct {
	Preset string `json:"preset,omitempty"`
	Read   Who    `json:"read,omitempty"`
	Add    Who    `json:"add,omitempty"`
	Edit   Who    `json:"edit,omitempty"`
	Delete Who    `json:"delete,omitempty"`
}

// Resolved is a checked matrix and what to call it: the preset's name, or
// "custom" with the preset it started from (Base, empty when none).
type Resolved struct {
	Matrix Matrix
	Preset string
	Base   string
}

// Resolve turns a spec into a checked matrix. A spec without a preset or
// any override takes fallback (the database default for a table, the
// default preset for a resource). A spec with overrides but no preset
// starts from fallback's matrix too.
func Resolve(s Spec, fallback Resolved) (Resolved, *RuleError) {
	base := s.Preset
	var m Matrix
	if base != "" {
		p, ok := Presets[base]
		if !ok {
			return Resolved{}, &RuleError{Code: "unknown_preset", Hint: "preset must be one of " + strings.Join(PresetOrder, ", ")}
		}
		m = p
	} else {
		m = fallback.Matrix
		base = fallback.Preset
		if base == "custom" {
			base = fallback.Base
		}
	}
	overridden := false
	for _, a := range Actions {
		var w Who
		switch a {
		case Read:
			w = s.Read
		case Add:
			w = s.Add
		case Edit:
			w = s.Edit
		case Delete:
			w = s.Delete
		}
		if w == "" {
			continue
		}
		if !w.Valid() {
			return Resolved{}, &RuleError{Code: "invalid_value", Hint: fmt.Sprintf("%s must be one of nobody, owner, own, signed-in or anyone", a)}
		}
		if w != m.Get(a) {
			overridden = true
		}
		m.set(a, w)
	}
	if err := Check(m); err != nil {
		return Resolved{}, err
	}
	if !overridden && base != "" && s.Preset != "" {
		return Resolved{Matrix: m, Preset: base}, nil
	}
	if !overridden && s.Preset == "" {
		return fallback, nil
	}
	if name := Name(m); name != "" {
		return Resolved{Matrix: m, Preset: name}, nil
	}
	return Resolved{Matrix: m, Preset: "custom", Base: base}, nil
}

// Default is the default preset, resolved.
func Default() Resolved { return Resolved{Matrix: Presets[DefaultPreset], Preset: DefaultPreset} }

// Name returns the preset whose matrix is m, or "".
func Name(m Matrix) string {
	for _, name := range PresetOrder {
		if Presets[name] == m {
			return name
		}
	}
	return ""
}

// Caller is who is asking, as the site-facing API sees them.
type Caller struct {
	// UserID is the signed-in person ("" for an anonymous visitor to a site
	// an admin opened to the network).
	UserID string
	// Owner is the site's owner or, for a team site, a member.
	Owner bool
	// Tool is a request from the owner's own tools (their key or the
	// connector), not from a page. Only the owner's tools reach nobody, and
	// they bypass the matrix.
	Tool bool
}

// Decision is what a caller may do with one action.
type Decision int

const (
	// Refused: not at all.
	Refused Decision = iota
	// Allowed: every entry.
	Allowed
	// OwnOnly: only entries the caller added.
	OwnOnly
)

// Decide applies one action's value to a caller. Anonymous visitors never
// change anything, whatever the value (the 2026-09-25 decision for
// network-open sites): add anyone takes adds from signed-in people only.
func Decide(m Matrix, a Action, c Caller) Decision {
	if c.Owner && c.Tool {
		return Allowed
	}
	w := m.Get(a)
	if c.UserID == "" {
		if a == Read && w == Anyone {
			return Allowed
		}
		return Refused
	}
	switch w {
	case Nobody:
		return Refused
	case Owner:
		if c.Owner {
			return Allowed
		}
		return Refused
	case Own:
		if c.Owner {
			return Allowed
		}
		return OwnOnly
	case SignedIn, Anyone:
		return Allowed
	}
	return Refused
}

// Column types.
const (
	TypeText    = "text"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeJSON    = "json"
)

// Column is one declared column of a table.
type Column struct {
	Type       string `json:"type"`
	Required   bool   `json:"required,omitempty"`
	References string `json:"references,omitempty"`
}

// UnmarshalJSON accepts "text" as well as {"type": "text", ...}.
func (c *Column) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = Column{Type: s}
		return nil
	}
	type plain Column
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("a column is a type name or {\"type\", \"required\", \"references\"}")
	}
	*c = Column(p)
	return nil
}

// ServerColumns are set by the server and never accepted from a request.
var ServerColumns = map[string]bool{"id": true, "added_by": true, "created_at": true, "updated_at": true, "mine": true}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ResourceNamePattern is a resource's name: what the URL carries.
var ResourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidIdentifier reports whether name may name a table or a column.
func ValidIdentifier(name string) bool { return namePattern.MatchString(name) }

// Limits on a database's shape.
const (
	MaxTables  = 20
	MaxColumns = 32
)

// CheckColumns validates one table's columns. tables is every table of the
// database, for references.
func CheckColumns(table string, cols map[string]Column, tables map[string]bool) error {
	if len(cols) == 0 {
		return fmt.Errorf("table %s needs at least one column", table)
	}
	if len(cols) > MaxColumns {
		return fmt.Errorf("table %s has more than %d columns", table, MaxColumns)
	}
	for _, name := range sortedKeys(cols) {
		col := cols[name]
		if !ValidIdentifier(name) {
			return fmt.Errorf("column %q in table %s: use lowercase letters, digits and _ (starting with a letter)", name, table)
		}
		if ServerColumns[name] {
			return fmt.Errorf("column %q in table %s is set by the server; leave it out", name, table)
		}
		switch col.Type {
		case TypeText, TypeInteger, TypeNumber, TypeBoolean, TypeJSON:
		default:
			return fmt.Errorf("column %s.%s: type must be text, integer, number, boolean or json", table, name)
		}
		if col.References != "" {
			if col.Type != TypeInteger {
				return fmt.Errorf("column %s.%s references a table, so its type must be integer (a row id)", table, name)
			}
			if !tables[col.References] {
				return fmt.Errorf("column %s.%s references %q, which is not a table of this database", table, name, col.References)
			}
		}
	}
	return nil
}

// RequiredError is a required column with no value (left out, or null).
type RequiredError struct{ Name string }

func (e *RequiredError) Error() string {
	return e.Name + " needs a value (required): include it, and do not send null for it"
}

// TypeWants says, per column type, what a value of that type looks like.
var TypeWants = map[string]string{
	TypeText:    `text in quotes, such as "bread"`,
	TypeInteger: "a whole number such as 42, without quotes",
	TypeNumber:  "a number such as 4.5, without quotes",
	TypeBoolean: "true or false, without quotes",
	TypeJSON:    "any JSON value",
}

// CheckValue validates one value against its column's type. JSON null is
// accepted for any column that is not required (*RequiredError otherwise).
// name is how the message names the column (the handler passes
// table.column).
func CheckValue(name string, col Column, raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		if col.Required {
			return &RequiredError{Name: name}
		}
		return nil
	}
	ok := true
	switch col.Type {
	case TypeText:
		var s string
		ok = json.Unmarshal(raw, &s) == nil
	case TypeInteger:
		var f float64
		ok = json.Unmarshal(raw, &f) == nil && f == math.Trunc(f) && math.Abs(f) <= 1<<53 && !strings.ContainsAny(trimmed, ".eE")
	case TypeNumber:
		var f float64
		ok = json.Unmarshal(raw, &f) == nil
	case TypeBoolean:
		var b bool
		ok = json.Unmarshal(raw, &b) == nil
	case TypeJSON:
		ok = json.Valid(raw)
	}
	if !ok {
		return fmt.Errorf("%s is %s: send %s", name, col.Type, TypeWants[col.Type])
	}
	return nil
}

// FilterValue turns a query-string value into the JSON a column holds, for
// an equality filter.
func FilterValue(name string, col Column, s string) (json.RawMessage, error) {
	var raw json.RawMessage
	switch col.Type {
	case TypeText:
		b, _ := json.Marshal(s)
		raw = b
	case TypeInteger, TypeNumber:
		raw = json.RawMessage(s)
	case TypeBoolean:
		if s != "true" && s != "false" {
			return nil, fmt.Errorf("where.%s: %s is boolean, so the value must be true or false", name, name)
		}
		raw = json.RawMessage(s)
	default:
		return nil, fmt.Errorf("where.%s: %s is a json column, which cannot be filtered; filter on a text, integer, number, or boolean column", name, name)
	}
	if CheckValue(name, col, raw) != nil {
		return nil, fmt.Errorf("where.%s: %s is %s, and %q is not a valid %s value", name, name, col.Type, s, col.Type)
	}
	// Numbers compare as jsonb numbers, so 7, 7.0 and 70e-1 match alike.
	return raw, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SortedKeys returns m's keys in order.
func SortedKeys[V any](m map[string]V) []string { return sortedKeys(m) }
