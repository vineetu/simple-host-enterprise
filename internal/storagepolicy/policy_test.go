package storagepolicy

import (
	"encoding/json"
	"testing"
)

// The reference: the four rules restated over plain strings, each by its
// own definition in the design, independent of Check and rank.
var widthOf = map[string]int{"nobody": 0, "owner": 1, "own": 2, "signed-in": 3, "anyone": 4}

func refRules(m [4]string) (r1, r2, r3, r4 bool) {
	read, add, edit, del := m[0], m[1], m[2], m[3]
	r1 = edit == "anyone" || del == "anyone"
	r2 = add == "own"
	usesOwn := read == "own" || add == "own" || edit == "own" || del == "own"
	r3 = usesOwn && add == "anyone"
	r4 = widthOf[edit] > widthOf[read] || widthOf[del] > widthOf[read]
	return
}

func every(f func(m [4]string)) {
	names := []string{"nobody", "owner", "own", "signed-in", "anyone"}
	for _, a := range names {
		for _, b := range names {
			for _, c := range names {
				for _, d := range names {
					f([4]string{a, b, c, d})
				}
			}
		}
	}
}

func TestRulesOverAll625Matrices(t *testing.T) {
	total, valid, n1, n2, n3, n4 := 0, 0, 0, 0, 0, 0
	every(func(m [4]string) {
		total++
		r1, r2, r3, r4 := refRules(m)
		if r1 {
			n1++
		}
		if r2 {
			n2++
		}
		if r3 {
			n3++
		}
		if r4 {
			n4++
		}
		wantValid := !(r1 || r2 || r3 || r4)
		got := Check(Matrix{Read: Who(m[0]), Add: Who(m[1]), Edit: Who(m[2]), Delete: Who(m[3])})
		if (got == nil) != wantValid {
			t.Errorf("Check(%v) = %v, want valid=%v", m, got, wantValid)
		}
		if wantValid {
			valid++
		}
	})
	// The counts the design states for hosted Simple Host.
	if total != 625 || valid != 161 || n1 != 225 || n2 != 125 || n3 != 61 || n4 != 350 {
		t.Fatalf("total %d valid %d R1 %d R2 %d R3 %d R4 %d; want 625 161 225 125 61 350", total, valid, n1, n2, n3, n4)
	}
}

func TestRuleCodes(t *testing.T) {
	for _, tc := range []struct {
		m    Matrix
		code string
	}{
		{Matrix{Anyone, Anyone, Anyone, Owner}, "anonymous_change"},
		{Matrix{Own, Own, Owner, Owner}, "add_own"},
		{Matrix{Own, Anyone, Owner, Owner}, "own_needs_signed_in_add"},
		{Matrix{Owner, SignedIn, SignedIn, Owner}, "blind_change"},
		{Matrix{"everyone", Owner, Owner, Owner}, "invalid_value"},
	} {
		got := Check(tc.m)
		if got == nil || got.Code != tc.code {
			t.Errorf("Check(%+v) = %v, want %s", tc.m, got, tc.code)
		}
	}
}

func TestPresetsAreValidAndDistinct(t *testing.T) {
	seen := map[Matrix]string{}
	for _, name := range PresetOrder {
		m, ok := Presets[name]
		if !ok {
			t.Fatalf("preset %s missing", name)
		}
		if err := Check(m); err != nil {
			t.Errorf("preset %s refused: %v", name, err)
		}
		if other, dup := seen[m]; dup {
			t.Errorf("presets %s and %s are the same matrix", name, other)
		}
		seen[m] = name
	}
	if len(Presets) != 7 {
		t.Fatalf("%d presets, want 7", len(Presets))
	}
	want := map[string][4]Who{
		"public":   {Anyone, Owner, Owner, Owner},
		"inbox":    {Owner, Anyone, Owner, Owner},
		"wall":     {Anyone, SignedIn, Owner, Own},
		"records":  {Own, SignedIn, Owner, Owner},
		"personal": {Own, SignedIn, Own, Own},
		"board":    {SignedIn, SignedIn, SignedIn, Owner},
		"private":  {Owner, Owner, Owner, Owner},
	}
	for name, w := range want {
		if Presets[name] != (Matrix{w[0], w[1], w[2], w[3]}) {
			t.Errorf("preset %s = %+v, want %v", name, Presets[name], w)
		}
	}
}

func TestResolve(t *testing.T) {
	r, err := Resolve(Spec{}, Default())
	if err != nil || r.Preset != "private" {
		t.Fatalf("empty spec = %+v %v", r, err)
	}
	r, err = Resolve(Spec{Preset: "inbox", Add: SignedIn}, Default())
	if err != nil || r.Preset != "custom" || r.Base != "inbox" || r.Matrix.Add != SignedIn || r.Matrix.Read != Owner {
		t.Fatalf("inbox+signed-in = %+v %v", r, err)
	}
	r, err = Resolve(Spec{Preset: "wall", Delete: Owner}, Default())
	if err != nil || r.Preset != "custom" || r.Base != "wall" {
		t.Fatalf("wall delete owner = %+v %v", r, err)
	}
	// Overrides that land on another preset are named for it.
	r, err = Resolve(Spec{Preset: "private", Read: Anyone}, Default())
	if err != nil || r.Preset != "public" {
		t.Fatalf("private read anyone = %+v %v", r, err)
	}
	if _, err := Resolve(Spec{Preset: "board", Edit: Anyone}, Default()); err == nil || err.Code != "anonymous_change" {
		t.Fatalf("board edit anyone = %v", err)
	}
	if _, err := Resolve(Spec{Preset: "everyone"}, Default()); err == nil || err.Code != "unknown_preset" {
		t.Fatalf("unknown preset = %v", err)
	}
	// A table with no spec takes the database default, custom included.
	db, _ := Resolve(Spec{Preset: "inbox", Add: SignedIn}, Default())
	r, err = Resolve(Spec{}, db)
	if err != nil || r != db {
		t.Fatalf("table inherits = %+v %v", r, err)
	}
	// An override on a custom default keeps its base.
	r, err = Resolve(Spec{Read: SignedIn}, db)
	if err != nil || r.Preset != "custom" || r.Base != "inbox" {
		t.Fatalf("override on custom = %+v %v", r, err)
	}
}

// refDecide restates who may act, per the design's "who is who" table.
func refDecide(who string, a Action, c Caller) Decision {
	if c.Tool && c.Owner {
		return Allowed
	}
	if c.UserID == "" { // anonymous: reads of anyone only
		if a == Read && who == "anyone" {
			return Allowed
		}
		return Refused
	}
	switch who {
	case "anyone", "signed-in":
		return Allowed
	case "own":
		if c.Owner {
			return Allowed
		}
		return OwnOnly
	case "owner":
		if c.Owner {
			return Allowed
		}
	}
	return Refused
}

func TestDecideEveryValueActionAndCaller(t *testing.T) {
	callers := map[string]Caller{
		"anonymous":      {},
		"colleague":      {UserID: "u1"},
		"colleague-tool": {UserID: "u1", Tool: true},
		"owner-on-site":  {UserID: "o", Owner: true},
		"owner-tool":     {UserID: "o", Owner: true, Tool: true},
	}
	for _, who := range Values {
		for _, a := range Actions {
			m := Matrix{}
			for _, b := range Actions {
				m.set(b, Owner)
			}
			m.set(a, who)
			for name, c := range callers {
				if got, want := Decide(m, a, c), refDecide(string(who), a, c); got != want {
					t.Errorf("Decide(%s %s, %s) = %v, want %v", a, who, name, got, want)
				}
			}
		}
	}
	// nobody is the owner's tools only.
	m := Matrix{Nobody, Nobody, Nobody, Nobody}
	if Decide(m, Read, callers["owner-on-site"]) != Refused || Decide(m, Read, callers["owner-tool"]) != Allowed {
		t.Fatal("nobody must admit the owner's tools only")
	}
}

func TestColumnsAndValues(t *testing.T) {
	tables := map[string]bool{"products": true, "orders": true}
	var cols map[string]Column
	if err := json.Unmarshal([]byte(`{"product_id":{"type":"integer","references":"products"},"qty":"integer","note":{"type":"text","required":true}}`), &cols); err != nil {
		t.Fatal(err)
	}
	if err := CheckColumns("orders", cols, tables); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"id":"text"}`, `{"Name":"text"}`, `{"x":"date"}`, `{"x":{"type":"text","references":"products"}}`,
		`{"x":{"type":"integer","references":"nope"}}`, `{}`, `{"x":{"type":"text","extra":1}}`,
	} {
		var c map[string]Column
		if err := json.Unmarshal([]byte(bad), &c); err != nil {
			continue
		}
		if err := CheckColumns("orders", c, tables); err == nil {
			t.Errorf("CheckColumns(%s) accepted", bad)
		}
	}
	for _, tc := range []struct {
		typ, raw string
		ok       bool
	}{
		{"text", `"hi"`, true}, {"text", `5`, false},
		{"integer", `5`, true}, {"integer", `5.5`, false}, {"integer", `5.0`, false}, {"integer", `"5"`, false}, {"integer", `1e3`, false},
		{"number", `5.5`, true}, {"number", `"x"`, false},
		{"boolean", `true`, true}, {"boolean", `1`, false},
		{"json", `{"a":[1]}`, true},
		{"text", `null`, true},
	} {
		err := CheckValue("c", Column{Type: tc.typ}, json.RawMessage(tc.raw))
		if (err == nil) != tc.ok {
			t.Errorf("CheckValue(%s, %s) = %v", tc.typ, tc.raw, err)
		}
	}
	if err := CheckValue("c", Column{Type: "text", Required: true}, json.RawMessage(`null`)); err == nil {
		t.Error("required null accepted")
	}
}
