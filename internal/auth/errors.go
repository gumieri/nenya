package auth

import (
	"fmt"
	"net/http"
)

// AuthError represents an authentication/authorization error with an HTTP status code.
type AuthError struct {
	Status int
	Msg    string
}

// Error implements the error interface.
func (e *AuthError) Error() string {
	return fmt.Sprintf("auth: status=%d msg=%s", e.Status, e.Msg)
}

var (
	ErrKeyDisabled = &AuthError{Status: http.StatusForbidden, Msg: "API key is disabled"}
	ErrKeyExpired  = &AuthError{Status: http.StatusForbidden, Msg: "API key has expired"}
)
