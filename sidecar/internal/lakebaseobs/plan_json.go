package lakebaseobs

import "encoding/json"

// MarshalJSON keeps the normalized plan as structured JSON at the history API.
// []byte's default base64 encoding would obscure the captured plan shape.
func (p Plan) MarshalJSON() ([]byte, error) {
	type fields Plan
	return json.Marshal(struct {
		fields
		JSON json.RawMessage
	}{fields: fields(p), JSON: json.RawMessage(p.JSON)})
}
