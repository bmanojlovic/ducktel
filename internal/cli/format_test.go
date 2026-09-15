package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
)

var (
	sampleRows = []map[string]interface{}{
		{"service_name": "svc-a", "count": int64(2)},
		{"service_name": "svc-b", "count": int64(1)},
	}
	sampleCols = []string{"service_name", "count"}
)

func TestFormatJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatResults(&buf, sampleRows, sampleCols, "json"); err != nil {
		t.Fatalf("FormatResults: %v", err)
	}

	var decoded []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(decoded) != 2 {
		t.Errorf("decoded %d rows, want 2", len(decoded))
	}
	if decoded[0]["service_name"] != "svc-a" {
		t.Errorf("first row = %v", decoded[0])
	}
}

// TestFormatJSONNullForNil verifies an empty result set encodes as null rather
// than erroring. This is what callers see for a query with no matches.
func TestFormatJSONNullForNil(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatResults(&buf, nil, sampleCols, "json"); err != nil {
		t.Fatalf("FormatResults(nil): %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "null" {
		t.Errorf("nil rows encoded as %q, want null", got)
	}
}

func TestFormatCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatResults(&buf, sampleRows, sampleCols, "csv"); err != nil {
		t.Fatalf("FormatResults: %v", err)
	}

	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\n%s", err, buf.String())
	}
	if len(records) != 3 {
		t.Fatalf("expected header + 2 rows, got %d", len(records))
	}
	if records[0][0] != "service_name" || records[0][1] != "count" {
		t.Errorf("header = %v", records[0])
	}
	if records[1][0] != "svc-a" || records[1][1] != "2" {
		t.Errorf("row 1 = %v", records[1])
	}
}

func TestFormatTable(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatResults(&buf, sampleRows, sampleCols, "table"); err != nil {
		t.Fatalf("FormatResults: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"service_name", "count", "svc-a", "svc-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
	// Header must come first.
	if !strings.HasPrefix(strings.TrimSpace(out), "service_name") {
		t.Errorf("table should start with the header row:\n%s", out)
	}
}

func TestFormatUnknownRejected(t *testing.T) {
	var buf bytes.Buffer
	err := FormatResults(&buf, sampleRows, sampleCols, "yaml")
	if err == nil {
		t.Error("unknown format should error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should name the bad format: %v", err)
	}
}

// TestFormatCSVEscapesCommas guards the reason CSV exists as an option at all:
// values containing delimiters must be quoted, not corrupt the row structure.
func TestFormatCSVEscapesCommas(t *testing.T) {
	rows := []map[string]interface{}{{"body": `has,comma and "quotes"`}}
	var buf bytes.Buffer
	if err := FormatResults(&buf, rows, []string{"body"}, "csv"); err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d: %v", len(records), records)
	}
	if records[1][0] != `has,comma and "quotes"` {
		t.Errorf("round-trip lost the value: %q", records[1][0])
	}
}
