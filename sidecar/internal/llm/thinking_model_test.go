package llm

import "testing"

func TestIsThinkingModel_Gemini3(t *testing.T) {
	thinking := []string{"gemini-2.5-flash", "gemini-3.5-flash", "gemini-3-flash-preview", "o1-mini", "o3"}
	for _, m := range thinking {
		if !isThinkingModel(m) {
			t.Errorf("isThinkingModel(%q) = false, want true", m)
		}
	}
	notThinking := []string{"gemini-2.0-flash", "gpt-4o-mini", "gemini-1.5-flash"}
	for _, m := range notThinking {
		if isThinkingModel(m) {
			t.Errorf("isThinkingModel(%q) = true, want false", m)
		}
	}
}
