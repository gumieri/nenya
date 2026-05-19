# Provider Service Kinds Matrix

This document provides a comprehensive overview of all supported providers and the service kinds (endpoints) they support within the Nenya gateway.

Note: Wire format capabilities (stream_options, tool_calls, reasoning, vision) are now **model-level** and inferred dynamically via `discovery.InferCapabilities()` from model IDs.

| Provider | Service Kinds | Notes |
|----------|--------------|-------|
| anthropic | llm | Full OpenAI↔Anthropic format conversion |
| azure | llm | Azure OpenAI endpoint |
| cohere | llm, rerank |  |
| deepinfra | llm |  |
| gemini | llm | Google-style dual auth (Authorization + x-goog-api-key) |
| github | llm | GitHub Models |
| groq | llm |  |
| mistral | llm |  |
| nvidia | llm |  |
| nvidia_free | llm |  |
| ollama | llm | Local inference |
| openai | llm, embedding, tts, stt, image, imageToText |  |
| openrouter | llm | Aggregator gateway |
| perplexity | llm, webSearch |  |
| qwen_free | llm |  |
| sambanova | llm |  |
| deepseek | llm | Requires `reasoning_content` on assistant messages |
| together | llm |  |
| xai | llm |  |
| zai | llm | Zhipu GLM - supports thinking mode |
| zen | llm |  |
