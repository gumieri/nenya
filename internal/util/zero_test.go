package util

import "testing"

func TestZeroBytes(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5}
	ZeroBytes(b)
	for i, v := range b {
		if v != 0 {
			t.Errorf("b[%d] = %d, want 0", i, v)
		}
	}
}

func TestZeroBytes_Nil(t *testing.T) {
	ZeroBytes(nil)
}

func TestZeroBytes_Empty(t *testing.T) {
	ZeroBytes([]byte{})
}
