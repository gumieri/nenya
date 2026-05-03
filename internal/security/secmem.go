package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
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
	sealed bool
}

func NewSecureMem(capacity int) (*SecureMem, error) {
	if capacity <= 0 {
		return nil, errors.New("secure memory capacity must be positive")
	}

	pageSize := syscall.Getpagesize()
	if capacity > math.MaxInt-pageSize+1 {
		return nil, fmt.Errorf("capacity %d too large for page-aligned allocation", capacity)
	}
	aligned := (capacity + pageSize - 1) & ^(pageSize - 1)

	data, err := syscall.Mmap(-1, 0, aligned, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, errors.New("secure memory allocation failed")
	}

	if err = syscall.Mlock(data); err != nil {
		_ = syscall.Munmap(data)
		if errors.Is(err, syscall.EPERM) {
			return nil, fmt.Errorf("%w: insufficient mlock limit (try increasing RLIMIT_MEMLOCK or run as root)", ErrMLockFailure)
		}
		return nil, fmt.Errorf("%w", ErrMLockFailure)
	}

	sm := &SecureMem{
		data: data,
		size: capacity,
	}

	return sm, nil
}

func (sm *SecureMem) StoreToken(token string) (SecureToken, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.sealed {
		return SecureToken{}, errors.New("cannot store token in sealed secure memory")
	}

	need := len(token)
	if need == 0 {
		return SecureToken{}, errors.New("cannot store empty token")
	}

	if need > sm.size-sm.used {
		return SecureToken{}, fmt.Errorf("secure memory overflow: used %d, size %d, need %d", sm.used, sm.size, need)
	}

	align := (need + 7) &^ 7
	if sm.used+align > sm.size {
		align = sm.size - sm.used
	}

	offset := sm.used
	copy(sm.data[offset:offset+need], token)
	sm.used += align

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
	if sm.data == nil || st.offset < 0 || st.offset+st.length > sm.size {
		return false
	}
	stored := make([]byte, st.length)
	copy(stored, sm.data[st.offset:st.offset+st.length])
	return subtle.ConstantTimeCompare(stored, []byte(input)) == 1
}

func (sm *SecureMem) GetToken(st SecureToken) ([]byte, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.data == nil || st.length == 0 || st.offset < 0 || st.offset+st.length > sm.size {
		return nil, false
	}
	out := make([]byte, st.length)
	copy(out, sm.data[st.offset:st.offset+st.length])
	return out, true
}

func (sm *SecureMem) Used() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.used
}

func (sm *SecureMem) Seal() error {
	if sm == nil {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.data == nil || sm.sealed {
		return nil
	}
	if err := syscall.Mprotect(sm.data, syscall.PROT_READ); err != nil {
		return fmt.Errorf("seal secure memory: %w", err)
	}
	sm.sealed = true
	return nil
}

func (sm *SecureMem) Destroy() {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.data != nil {
		if sm.sealed {
			_ = syscall.Mprotect(sm.data, syscall.PROT_READ|syscall.PROT_WRITE)
		}
		for i := range sm.data {
			sm.data[i] = 0
		}
		runtime.KeepAlive(sm.data)
		sm.sealed = false
		_ = syscall.Munmap(sm.data)
		sm.data = nil
		sm.used = 0
	}
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
	if numKeys < 0 || providerKeyCount < 0 {
		return 0
	}
	if numKeys > math.MaxInt-providerKeyCount {
		return math.MaxInt
	}
	return (numKeys + providerKeyCount) * (avgTokenLen + 8)
}
