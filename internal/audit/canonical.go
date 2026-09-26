package audit

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// chainedEvent is one audit_events row as the chain hashes it, read raw
// (no database function applied) so audit-verify recomputes the hash in Go.
// The database owner can redefine audit_event_canonical, but not this.
type chainedEvent struct {
	ID              int64
	At              time.Time
	RequestID       sql.NullString
	ActorID         sql.NullString
	ActorKind       string
	KeyID           sql.NullString
	Action          string
	OwnerID         sql.NullString
	SiteID          sql.NullString
	TeamID          sql.NullString
	ViaSiteLabel    sql.NullString
	ViaSiteName     sql.NullString
	ViaSiteObserved bool
	IP              sql.NullString // ip::text, which always carries the mask ("10.0.0.9/32")
	UserAgent       sql.NullString
	Detail          []byte // jsonb
}

// canonical is the Go twin of migration 0036's audit_event_canonical: the
// jsonb_build_object of the row's fields, rendered as Postgres renders
// jsonb as text. The two must agree byte for byte; chain_db_test.go checks
// them against each other on a real database.
func (e chainedEvent) canonical() ([]byte, error) {
	var detail any
	if e.Detail != nil {
		dec := json.NewDecoder(bytes.NewReader(e.Detail))
		dec.UseNumber()
		if err := dec.Decode(&detail); err != nil {
			return nil, fmt.Errorf("event %d detail: %w", e.ID, err)
		}
		if e.Action == "state_write" {
			detail = jsonbMinusKey(detail, "count")
		}
	}
	str := func(s sql.NullString) any {
		if !s.Valid {
			return nil
		}
		return s.String
	}
	obj := map[string]any{
		"id":                json.Number(strconv.FormatInt(e.ID, 10)),
		"at":                e.At.UTC().Format("2006-01-02T15:04:05.000000Z"),
		"request_id":        str(e.RequestID),
		"actor_id":          str(e.ActorID),
		"actor_kind":        e.ActorKind,
		"key_id":            str(e.KeyID),
		"action":            e.Action,
		"owner_id":          str(e.OwnerID),
		"site_id":           str(e.SiteID),
		"team_id":           str(e.TeamID),
		"via_site_label":    str(e.ViaSiteLabel),
		"via_site_name":     str(e.ViaSiteName),
		"via_site_observed": e.ViaSiteObserved,
		"ip":                str(e.IP),
		"user_agent":        str(e.UserAgent),
		"detail":            detail,
	}
	var buf bytes.Buffer
	if err := writeJSONB(&buf, obj); err != nil {
		return nil, fmt.Errorf("event %d: %w", e.ID, err)
	}
	return buf.Bytes(), nil
}

// jsonbMinusKey is Postgres's `jsonb - text`: an object loses that key, an
// array loses the string elements equal to it.
func jsonbMinusKey(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, key)
	case []any:
		out := t[:0]
		for _, el := range t {
			if s, ok := el.(string); ok && s == key {
				continue
			}
			out = append(out, el)
		}
		return out
	}
	return v
}

// writeJSONB renders v the way Postgres's jsonb output does: object keys
// ordered by length and then bytewise, ", " between elements and ": " after
// a key, numbers in numeric's plain notation, and only the escapes
// Postgres's escape_json makes.
func writeJSONB(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeJSONBString(buf, t)
	case json.Number:
		n, err := numericText(string(t))
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case []any:
		buf.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				buf.WriteString(", ")
			}
			if err := writeJSONB(buf, el); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) < len(keys[j])
			}
			return keys[i] < keys[j]
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeJSONBString(buf, k)
			buf.WriteString(": ")
			if err := writeJSONB(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unexpected json value %T", v)
	}
	return nil
}

// writeJSONBString is Postgres's escape_json.
func writeJSONBString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		default:
			if c < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, c)
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}

// numericText renders a JSON number as Postgres's numeric output does: plain
// decimal notation, with as many fraction digits as the input's own scale
// less its exponent (1.50e1 is 15.0, 1e3 is 1000, 1.5e-3 is 0.0015), and no
// sign on zero.
func numericText(s string) (string, error) {
	neg := strings.HasPrefix(s, "-")
	mant := strings.TrimPrefix(s, "-")
	exp := 0
	if i := strings.IndexAny(mant, "eE"); i >= 0 {
		var err error
		if exp, err = strconv.Atoi(mant[i+1:]); err != nil {
			return "", fmt.Errorf("number %q: %w", s, err)
		}
		mant = mant[:i]
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	scale := len(frac) - exp
	if scale < 0 {
		digits += strings.Repeat("0", -scale)
		scale = 0
	}
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	whole := strings.TrimLeft(digits[:len(digits)-scale], "0")
	if whole == "" {
		whole = "0"
	}
	out := whole
	if scale > 0 {
		out += "." + digits[len(digits)-scale:]
	}
	if neg && strings.Trim(digits, "0") != "" {
		out = "-" + out
	}
	return out, nil
}
