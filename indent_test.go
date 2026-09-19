package agenttool

import (
	"bytes"
	"encoding/json"
)

// indentJSON pretty-prints raw while keeping its key order.
func indentJSON(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
