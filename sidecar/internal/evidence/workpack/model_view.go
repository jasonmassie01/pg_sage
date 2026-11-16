package workpack

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// TierView is a model tier as a reader receives it (the API's tier=model
// and MCP's get_workload_pack): its JSON sections as they are, its charts
// as PNG data URIs, and its size: the bytes a model would read and a rough
// token estimate (4 bytes a token), the input to a review's cost.
type TierView struct {
	Tier           string                     `json:"tier"`
	ContentSHA256  string                     `json:"content_sha256"`
	Manifest       Manifest                   `json:"manifest"`
	Sections       map[string]json.RawMessage `json:"sections"`
	Charts         map[string]string          `json:"charts"`
	Bytes          int                        `json:"bytes"`
	TokensEstimate int                        `json:"tokens_estimate"`
}

// ViewOf is m's view. The pseudonym map is not part of it.
func ViewOf(m *Pack) TierView {
	v := TierView{Tier: m.Manifest.Tier, ContentSHA256: m.ContentSHA256, Manifest: m.Manifest,
		Sections: map[string]json.RawMessage{}, Charts: map[string]string{}}
	for _, f := range m.Files {
		v.Bytes += len(f.Data)
		if strings.HasPrefix(f.Name, chartDir) {
			v.Charts[f.Name] = "data:image/png;base64," +
				base64.StdEncoding.EncodeToString(f.Data)
			continue
		}
		v.Sections[f.Name] = json.RawMessage(f.Data)
	}
	v.TokensEstimate = v.Bytes / 4
	return v
}
