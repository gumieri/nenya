package routing

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

// TurnFingerprint is a SHA-256 digest of one canonicalized conversation turn.
type TurnFingerprint [sha256.Size]byte

// Affinity chain bounds. The chain stores prefix hashes of the leading turns
// only: its purpose (NENYA-39) is distinguishing conversations that share a
// generic opening turn, NOT representing whole histories — nenya's session
// identity root is the first user turn (stable as conversations grow), so a
// bounded leading-window fingerprint set is sufficient and keeps memory at
// ~2KB per remembered session.
const (
	// affinityChainDepth caps how many leading turns are fingerprinted.
	affinityChainDepth = 64
	// affinitySampleBytes is the per-part byte budget: parts at or below this
	// size are hashed whole; larger parts contribute a head/length/tail
	// sample so a huge history costs O(turns × sample), never O(history).
	affinitySampleBytes = 16 << 10
	affinitySampleHead  = 4 << 10
	affinitySampleTail  = 4 << 10
	// DefaultAffinityCap / DefaultAffinityTTL bound the flat KV store.
	DefaultAffinityCap = 8192
	DefaultAffinityTTL = time.Hour
)

// affinityEntry remembers one session's leading-turn prefix chain.
type affinityEntry struct {
	key      string
	chain    []TurnFingerprint
	accessed time.Time
	element  *list.Element
}

// AffinityStore is a bounded flat KV (session key → leading-turn fingerprint
// chain) used to detect when a request's stable session key collides with a
// different conversation sharing the same opening turn (NENYA-39). No
// materialized trees: entries are flat, LRU-evicted, TTL-bounded, and the
// fork of a colliding conversation derives deterministically from its own
// fingerprint chain, so repeat requests of the fork stay stable without a
// parent lookup.
type AffinityStore struct {
	mu      sync.Mutex
	cap     int
	ttl     time.Duration
	entries map[string]*affinityEntry
	lru     *list.List
}

// NewAffinityStore creates an affinity store with the given entry cap and
// TTL (non-positive values fall back to the defaults).
func NewAffinityStore(cap int, ttl time.Duration) *AffinityStore {
	if cap <= 0 {
		cap = DefaultAffinityCap
	}
	if ttl <= 0 {
		ttl = DefaultAffinityTTL
	}
	return &AffinityStore{
		cap:     cap,
		ttl:     ttl,
		entries: make(map[string]*affinityEntry),
		lru:     list.New(),
	}
}

// BuildAffinityChain computes the prefix-fingerprint chain over the leading
// turns of a chat payload: h_k = H(domain || h_{k-1} || fingerprint(turn_k)).
// Each turn contributes its role plus bounded samples of its content parts,
// so cost is linear in turn count and independent of total history size.
// Deterministic for identical payloads.
func BuildAffinityChain(payload map[string]any) []TurnFingerprint {
	if payload == nil {
		return nil
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	if len(messages) > affinityChainDepth {
		messages = messages[:affinityChainDepth]
	}
	chain := make([]TurnFingerprint, 0, len(messages))
	var prev []byte
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		h := sha256.New()
		h.Write([]byte("nenya-affinity-turn-v1"))
		h.Write([]byte{0})
		h.Write([]byte(role))
		for _, part := range canonicalContentParts(msg["content"]) {
			h.Write([]byte{1})
			h.Write(part)
		}
		chain = append(chain, chainStep(h.Sum(nil), prev))
		prev = chain[len(chain)-1][:]
	}
	return chain
}

// canonicalContentParts flattens a message content field into bounded byte
// samples: string content contributes one sample; OpenAI-style part arrays
// contribute their text parts (type-tagged) so structurally different
// payloads never collide.
func canonicalContentParts(content any) [][]byte {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return [][]byte{sampleBytes([]byte(c))}
	case []any:
		parts := make([][]byte, 0, len(c))
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			text, _ := part["text"].(string)
			if typ == "" && text == "" {
				continue
			}
			sampled := sampleBytes([]byte(typ + "\x00" + text))
			parts = append(parts, sampled)
		}
		return parts
	default:
		return nil
	}
}

// sampleBytes bounds the hash input for oversized parts: head sample, the
// full byte length (8 bytes big-endian), and the tail sample.
func sampleBytes(b []byte) []byte {
	if len(b) <= affinitySampleBytes {
		return b
	}
	out := make([]byte, 0, affinitySampleHead+8+affinitySampleTail)
	out = append(out, b[:affinitySampleHead]...)
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	out = append(out, lenBuf[:]...)
	out = append(out, b[len(b)-affinitySampleTail:]...)
	return out
}

// chainStep folds a turn fingerprint into the prefix chain.
func chainStep(turnFP []byte, prev []byte) TurnFingerprint {
	h := sha256.New()
	h.Write([]byte("nenya-affinity-chain-v1"))
	if prev != nil {
		h.Write(prev)
	}
	h.Write(turnFP)
	var out TurnFingerprint
	copy(out[:], h.Sum(nil))
	return out
}

// lcpTurns returns the number of shared leading fingerprints.
func lcpTurns(a, b []TurnFingerprint) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// ResolveIdentity returns the session identity for a request whose stable
// root key and leading-turn chain are given. An exact entry whose chain
// still matches (shared prefix ≥ min(2, remembered depth)) keeps the root
// identity and extends the remembered chain. A collision — same opening
// turn, different continuation — forks a deterministic identity derived
// from the divergent chain, leaving the original session untouched. Fresh
// keys register a new entry.
func (s *AffinityStore) ResolveIdentity(rootKey string, chain []TurnFingerprint) string {
	if s == nil || rootKey == "" {
		return rootKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()

	if entry, ok := s.entries[rootKey]; ok {
		shared := lcpTurns(entry.chain, chain)
		// A remembered depth of 1 (conversation just started) matches on the
		// single leading turn; deeper histories require ≥2 shared turns so a
		// different conversation with the same opener forks instead.
		if shared >= min(2, len(entry.chain)) {
			entry.chain = chain
			entry.accessed = time.Now()
			s.lru.MoveToFront(entry.element)
			return rootKey
		}
		// The fork identity folds the SHARED prefix only, so it stays
		// stable as the fork grows beyond the branch point (the LCP against
		// the original entry does not change when only the fork advances).
		forkKey := "fork-" + hexFingerprint(chain[:shared]) + "-" + rootKey
		if _, ok := s.entries[forkKey]; !ok {
			s.insertLocked(forkKey, chain)
		} else {
			fork := s.entries[forkKey]
			fork.chain = chain
			fork.accessed = time.Now()
			s.lru.MoveToFront(fork.element)
		}
		return forkKey
	}

	s.insertLocked(rootKey, chain)
	return rootKey
}

// insertLocked adds an entry and evicts LRU/t expired entries over cap.
func (s *AffinityStore) insertLocked(key string, chain []TurnFingerprint) {
	entry := &affinityEntry{key: key, chain: chain, accessed: time.Now()}
	entry.element = s.lru.PushFront(entry)
	s.entries[key] = entry
	for len(s.entries) > s.cap {
		oldest := s.lru.Back()
		if oldest == nil {
			return
		}
		evicted := oldest.Value.(*affinityEntry)
		s.lru.Remove(oldest)
		delete(s.entries, evicted.key)
	}
}

// evictExpiredLocked drops entries idle past the TTL.
func (s *AffinityStore) evictExpiredLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-s.ttl)
	for key, entry := range s.entries {
		if entry.accessed.Before(cutoff) {
			s.lru.Remove(entry.element)
			delete(s.entries, key)
		}
	}
}

// Len reports the number of remembered sessions (test/observability use).
func (s *AffinityStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func hexFingerprint(chain []TurnFingerprint) string {
	const hexDigits = "0123456789abcdef"
	if len(chain) == 0 {
		return "empty"
	}
	// Fold the given prefix so the fork key is stable across continued
	// growth of the same divergent conversation.
	h := sha256.New()
	for _, fp := range chain {
		h.Write(fp[:])
	}
	sum := h.Sum(nil)
	out := make([]byte, 16)
	for i, b := range sum[:8] {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out)
}
