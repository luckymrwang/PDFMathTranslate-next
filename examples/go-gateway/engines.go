package main

import (
	"net/http"
	"os"
)

// Engine settings are resolved on the server: API keys must never be sent by
// the mini-program or accepted from its multipart request.
func availableEngines() map[string]map[string]string {
	engines := map[string]map[string]string{
		"Google": {"translate_engine_type": "Google"},
		"Bing":   {"translate_engine_type": "Bing"},
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		engines["GPT-6"] = map[string]string{
			"translate_engine_type": "OpenAI",
			"openai_api_key":        key,
			"openai_model":          getenv("OPENAI_MODEL", "gpt-6-luna"),
		}
		if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
			engines["GPT-6"]["openai_base_url"] = base
		}
	}
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		engines["DeepSeek-V4.1-Flash"] = map[string]string{
			"translate_engine_type":  "DeepSeek",
			"deepseek_api_key":       key,
			"deepseek_model":         getenv("DEEPSEEK_MODEL", "deepseek-flash"),
			"deepseek_thinking_mode": "disabled",
		}
	}
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		engines["Gemini 2.5 Pro"] = map[string]string{
			"translate_engine_type": "Gemini",
			"gemini_api_key":        key,
			"gemini_model":          getenv("GEMINI_MODEL", "gemini-2.5-pro"),
		}
	}
	// ClaudeCode uses an installed CLI, not a direct Anthropic API key.
	if path := os.Getenv("CLAUDE_CODE_PATH"); path != "" {
		engines["Claude 3.7 Sonnet"] = map[string]string{
			"translate_engine_type": "ClaudeCode",
			"claude_code_path":      path,
			"claude_code_model":     getenv("CLAUDE_CODE_MODEL", "claude-3-7-sonnet-20250219"),
		}
	}
	return engines
}

func (s *Server) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	available := availableEngines()
	names := make([]string, 0, len(available))
	for _, name := range []string{"Bing", "Google", "GPT-6", "Claude 3.7 Sonnet", "DeepSeek-V4.1-Flash", "Gemini 2.5 Pro"} {
		if _, ok := available[name]; ok {
			names = append(names, name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engines": names})
}
