package oidc

import (
	"errors"
	"fmt"
	"strings"
)

// The verifier's failures fall in three classes that callers must map to
// different HTTP statuses, so they are distinguishable with errors.Is.
var (
	// ErrInvalidToken: the token is bad, expired, or not meant for us (401).
	ErrInvalidToken = errors.New("oidc: invalid token")
	// ErrInsufficientScope: the token is fine but lacks a required scope (403).
	ErrInsufficientScope = errors.New("oidc: insufficient scope")
	// ErrProviderUnavailable: the token could not be judged because the
	// provider's discovery, JWKS or introspection endpoint failed (503).
	ErrProviderUnavailable = errors.New("oidc: provider unavailable")
)

// ScopeError is an ErrInsufficientScope that names the missing scopes.
type ScopeError struct {
	Missing []string
}

func (e *ScopeError) Error() string {
	return "oidc: token missing required scope(s): " + strings.Join(e.Missing, ", ")
}

func (e *ScopeError) Is(target error) bool { return target == ErrInsufficientScope }

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(format, args...))
}

func unavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
}
