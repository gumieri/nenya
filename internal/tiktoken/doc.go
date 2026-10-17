// Package tiktoken implements BPE (Byte Pair Encoding) token counting using
// the cl100k_base vocabulary embedded at build time. It is used to estimate
// token counts for context window management and payload size decisions without
// calling an external tokenizer.
//
// CountTokens is the primary path and requires no network access. Two helpers
// cover what the raw encoder cannot (NENYA-157):
//
//   - EstimateByClass answers when the embedded vocabulary cannot be loaded
//     (the checksum gate rejects a corrupted or tampered binary), where the
//     alternative is a zero estimate and therefore unbounded budgets. It is a
//     class-weighted approximation calibrated against the real encoder, and it
//     deliberately over-estimates the classes where that is achievable. Its
//     weights, per RUN of one class:
//
//   - ASCII letter/digit runs: ~5 characters per token, unless the run is
//     high-entropy — a letter/digit mix, a pure number, or a run longer than
//     16 characters with fewer than one vowel per four (a shape no word takes)
//     — which costs ~2 characters per token, or ~3 for a long pure number
//     (the encoder groups digits in threes).
//
//   - Punctuation runs: ~3 characters per token.
//
//   - CJK/kana/hangul: one token per character.
//
//   - Other non-ASCII letters (Cyrillic, Greek, accented Latin): one per two.
//
//   - Emoji: three, plus one per modifier (skin tone, variation selector,
//     zero-width joiner).
//
//   - Space/tab runs: one token up to 20 characters, except a single space
//     separating two words, which the encoder absorbs into the next token.
//
//   - Newlines: one each.
//
//   - ImageTokens prices a vision block by pixel area (Anthropic's published
//     (width x height) / 750), reading the dimensions out of the container
//     header inside the inlined base64 through a bounded probe: no pixel
//     decode and no network. An unmeasurable image costs a documented default
//     rather than zero.
package tiktoken
