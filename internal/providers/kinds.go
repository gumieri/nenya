package providers

type ServiceKind string

const (
	ServiceKindLLM         ServiceKind = "llm"
	ServiceKindEmbedding   ServiceKind = "embedding"
	ServiceKindTTS         ServiceKind = "tts"
	ServiceKindSTT         ServiceKind = "stt"
	ServiceKindImage       ServiceKind = "image"
	ServiceKindImageToText ServiceKind = "imageToText"
	ServiceKindWebSearch   ServiceKind = "webSearch"
	ServiceKindWebFetch    ServiceKind = "webFetch"
	ServiceKindRerank      ServiceKind = "rerank"
	ServiceKindAudio       ServiceKind = "audio"
	ServiceKindFiles       ServiceKind = "files"
	ServiceKindBatches     ServiceKind = "batches"
	ServiceKindModerations ServiceKind = "moderations"
)

type EndpointMapping struct {
	Method string
	Path   string
}

var ServiceKindEndpoints = map[ServiceKind]EndpointMapping{
	ServiceKindEmbedding:   {Method: "POST", Path: "/v1/embeddings"},
	ServiceKindTTS:         {Method: "POST", Path: "/v1/audio/speech"},
	ServiceKindSTT:         {Method: "POST", Path: "/v1/audio/transcriptions"},
	ServiceKindImage:       {Method: "POST", Path: "/v1/images/generations"},
	ServiceKindRerank:      {Method: "POST", Path: "/v1/rerank"},
	ServiceKindFiles:       {Method: "POST", Path: "/v1/files"},
	ServiceKindBatches:     {Method: "POST", Path: "/v1/batches"},
	ServiceKindModerations: {Method: "POST", Path: "/v1/moderations"},
}

// Supports returns true if the provider supports the given service kind.
func (s *ProviderSpec) Supports(kind ServiceKind) bool {
	for _, k := range s.ServiceKinds {
		if k == kind {
			return true
		}
	}
	return false
}
