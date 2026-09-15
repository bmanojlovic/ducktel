package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// marshalRows renders query results as indented JSON, preserving column order.
//
// ducktel's engine returns rows as maps, which lose column order, so the order
// it separately reports is used to rebuild each row. Go's encoding/json sorts
// map keys, which would otherwise present columns alphabetically rather than in
// the order the query asked for.
func marshalRows(rows []map[string]any, cols []string) (string, error) {
	if len(rows) == 0 {
		return "[]", nil
	}

	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, row := range rows {
		if i > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString("  {")
		for j, c := range cols {
			if j > 0 {
				buf.WriteString(", ")
			}
			key, err := json.Marshal(c)
			if err != nil {
				return "", fmt.Errorf("marshalling column name: %w", err)
			}
			val, err := json.Marshal(row[c])
			if err != nil {
				// A non-marshalable value (NaN/Inf from a metric, say) would
				// otherwise fail the whole result set, so degrade to null.
				val = []byte("null")
			}
			buf.Write(key)
			buf.WriteString(": ")
			buf.Write(val)
		}
		buf.WriteString("}")
	}
	buf.WriteString("\n]")
	return buf.String(), nil
}
