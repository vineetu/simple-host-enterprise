package handler

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
)

func TestExportCSVNeutralisesFormulas(t *testing.T) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := writeAccessLogCSVRow(w, accessLogEntryResponse{Path: "/=HYPERLINK(\"x\")", UserAgent: "@SUM(1)", Method: "GET", OwnerLabel: "-2+3", SiteName: "\tx", ClientKind: "human"}); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	row, err := csv.NewReader(strings.NewReader(buf.String())).Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range row {
		if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
			t.Fatalf("cell %q still starts with a formula character (row %q)", cell, row)
		}
	}
	if !strings.Contains(buf.String(), "'@SUM(1)") || !strings.Contains(buf.String(), "GET") {
		t.Fatalf("row = %q", buf.String())
	}
}
