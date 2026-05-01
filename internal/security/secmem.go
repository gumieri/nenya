package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
)

var ErrMLockFailure = errors.New("secure memory allocation failed")

type SecureToken struct {
	offset int
	length int
}

type SecureMem struct {
	mu     sync.Mutex
	data   []byte
	used   int
	size   int
	locked bool
}

func NewSecureMem(capacity int) (*SecureMem, error) {
	sm := &SecureMem{
		size: capacity,
	}

	if capacity <= 0 {
		return nil, errors.New("secure memory capacity must be positive")
	}

	pageSize := syscall.Getpagesize()
	aligned := (capacity + pageSize - 1) & ^(pageSize - 1)

	data, err := syscall.Mmap(-1, 0, aligned, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, errors.New("secure memory allocation failed")
	}

	if err = syscall.Mlock(data); err != nil {
		_ = syscall.Munmap(data)
		return nil, fmt.Errorf("%w", ErrMLockFailure)
	}

	sm.data = data
	sm.locked = true

	return sm, nil
}

func (sm *SecureMem) StoreToken(token string) (SecureToken, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	need := len(token)
	if need == 0 {
		return SecureToken{}, errors.New("cannot store empty token")
	}

	align := (need + 7) &^ 7
	if sm.used+align > sm.size {
		return SecureToken{}, fmt.Errorf("secure memory overflow: used %d, size %d, need %d", sm.used, sm.size, need)
	}

	offset := sm.used
	copy(sm.data[offset:offset+need], token)
	sm.used += align

	runtime.KeepAlive(sm.data)

	return SecureToken{offset: offset, length: need}, nil
}

func (sm *SecureMem) CompareToken(st SecureToken, input string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if input == "" || st.length == 0 {
		return false
	}
	if len(input) != st.length {
		return false
	}
	if sm.data == nil || st.offset < 0 || st.offset+st.length > len(sm.data) {
		return false
	}
	stored := make([]byte, st.length)
	copy(stored, sm.data[st.offset:st.offset+st.length])
	return subtle.ConstantTimeCompare(stored, []byte(input)) == 1
}

func (sm *SecureMem) Destroy() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.data != nil {
		for i := range sm.data {
			sm.data[i] = 0
		}
		runtime.KeepAlive(sm.data)
		sm.locked = false
		_ = syscall.Munmap(sm.data)
		sm.data = nil
		sm.used = 0
	}
}

func (sm *SecureMem) Used() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.used
}

func GenerateToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("failed to read random: " + err.Error())
	}
	return "nk-" + hex.EncodeToString(b)
}

func TokenSizeHint(numKeys int, providerKeyCount int) int {
	const avgTokenLen = 64
	return (numKeys + providerKeyCount) * (avgTokenLen + 8)
}
