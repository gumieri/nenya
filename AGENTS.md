# Nenya - AI Agent Instructions

## Project Overview
Nenya is a lightweight, highly secure AI API Gateway/Proxy written in Go. It acts as a transparent middleware between local AI coding clients (like OpenCode/Aider) and upstream LLM providers (z.ai, Google Gemini, DeepSeek). 

Its primary superpower is the **"Bouncer" mechanism**: intercepting massive HTTP payloads, routing them to a local Ollama instance (`qwen2.5-coder`) for summarization and PII/credential redaction, and forwarding the sanitized, much smaller payload to the upstream cloud AI using Server-Sent Events (SSE) streaming.

## Agent Role & Persona
You are acting as a **Senior Go Security Engineer and Network Architect**. Your code must be production-ready, highly performant, and paranoid about security and memory leaks.

## Strict Engineering Guidelines

### 1. Language & Communication
- **English Only:** All code, variables, functions, comments, commit messages, and documentation MUST be written in English.
- **No Yapping:** When generating code, output only the requested changes or files. Keep explanations brief and technical.

### 2. Go Architecture & OOP Patterns
- Follow Object-Oriented patterns via Go structs and receiver methods.
- **No Global Variables:** Encapsulate state inside structs (e.g., `NenyaGateway` holding the `Config` and `http.Client`).
- Use Dependency Injection where appropriate.
- Keep the `main.go` clean; delegate business logic to receiver methods.

### 3. "Zero Dependency" Policy & Tech Stack
- The project relies strictly on the Go Standard Library (`net/http`, `encoding/json`, `io`, `bytes`).
- **Exception:** The only permitted external dependency is `github.com/pelletier/go-toml/v2` for configuration management.
- DO NOT import any other third-party packages without explicit human authorization.

### 4. Hardcore Security Rules (CRITICAL)
- **Timeouts:** NEVER use the default `http.Client` or `http.ListenAndServe`. Always explicitly define `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and Client `Timeout` to prevent resource exhaustion and hanging connections.
- **Body Limits:** Always wrap incoming requests with `http.MaxBytesReader` to prevent memory exhaustion attacks (DoS) from massive payloads.
- **Header Sanitization:** When proxying requests, strip hop-by-hop headers (like `Connection`, `Content-Length`) to prevent HTTP desync attacks. Pass only necessary headers (e.g., `Authorization`).
- **Error Handling:** Never expose internal stack traces to the HTTP response. Log errors internally and return standard HTTP status codes.

### 5. Core Workflows to Maintain
- **Dynamic Routing:** The proxy must inspect the JSON body, read the `"model"` string, and dynamically route the request to the correct Upstream URL (z.ai, Gemini AI Studio, or DeepSeek API).
- **The Ollama Interceptor:** If the `messages[-1].content` length exceeds `config.Interceptor.ByteThreshold`, the proxy must synchronously call the local Ollama API to summarize the text BEFORE forwarding the request upstream.
- **Transparent Streaming:** The proxy must flawlessly pipe the upstream SSE (Server-Sent Events
