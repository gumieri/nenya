package providers

// XaiSanitize applies xAI-specific request sanitization:
// - Injects reasoning_effort for reasoning-capable Grok models if not set
func XaiSanitize(deps *SanitizeDeps, payload map[string]interface{}) {
	modelRaw, ok := payload["model"]
	if !ok {
		return
	}
	model, ok := modelRaw.(string)
	if !ok || model == "" {
		return
	}

	if deps.SupportsReasoning == nil || !deps.SupportsReasoning(model) {
		return
	}

	if _, hasEffort := payload["reasoning_effort"]; !hasEffort {
		payload["reasoning_effort"] = "medium"
	}
}
