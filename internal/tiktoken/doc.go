// Package tiktoken implements BPE (Byte Pair Encoding) token counting
// using the cl100k_base vocabulary embedded at build time. It is used to
// estimate token counts for context window management and payload size
// decisions without calling an external tokenizer.
package tiktoken
