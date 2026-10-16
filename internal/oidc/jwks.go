package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/jwkset"
	"golang.org/x/sync/singleflight"
)

// jwksKey is one usable verification key from the provider's JWKS.
type jwksKey struct {
	kid string
	alg string // the JWK's own "alg", if it declares one
	key any
}

// jwksCache holds the provider's signing keys. Unlike a generic JWKS client
// it distinguishes "the provider failed" from "this kid does not exist", which
// is what lets the server answer 503 instead of 401 during an IdP outage.
type jwksCache struct {
	mu                 sync.Mutex
	keys               []jwksKey
	fetchedAt          time.Time
	lastUnknownRefresh time.Time // last refresh an unknown kid asked for
	flight             singleflight.Group
}

// lookup finds the key for a token's kid/alg, refreshing the JWKS when it is
// older than ttl or when the kid is unknown. An unknown kid triggers at most
// one refresh per cooldown, so a flood of random kids cannot turn into a flood
// of provider requests.
func (v *Verifier) lookupKey(ctx context.Context, jwksURI, kid, alg string) (jwksKey, error) {
	c := &v.jwks
	now := v.now()

	c.mu.Lock()
	keys, fetchedAt := c.keys, c.fetchedAt
	fresh := !fetchedAt.IsZero() && now.Sub(fetchedAt) < v.CacheTTL
	if fresh {
		if k, ok := matchKey(keys, kid, alg); ok {
			c.mu.Unlock()
			return k, nil
		}
		// Fresh key set, unknown kid: refresh unless one was just done for
		// the same reason. (A never-loaded or expired set always refreshes.)
		if now.Sub(c.lastUnknownRefresh) < v.unknownKIDCooldown {
			c.mu.Unlock()
			return jwksKey{}, invalidf("no signing key for kid %q", kid)
		}
		c.lastUnknownRefresh = now
	}
	c.mu.Unlock()

	keys, err := v.refreshJWKS(ctx, jwksURI)
	if err != nil {
		// Keep serving from a stale key set rather than failing every
		// request because the provider is briefly down.
		if k, ok := matchKey(c.snapshot(), kid, alg); ok {
			return k, nil
		}
		return jwksKey{}, err
	}
	if k, ok := matchKey(keys, kid, alg); ok {
		return k, nil
	}
	return jwksKey{}, invalidf("no signing key for kid %q", kid)
}

func (c *jwksCache) snapshot() []jwksKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys
}

func (v *Verifier) refreshJWKS(ctx context.Context, jwksURI string) ([]jwksKey, error) {
	res, err, _ := v.jwks.flight.Do("jwks", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()

		var set jwkset.JWKSMarshal
		err := fetchJSON(ctx, v.client(), jwksURI, maxJWKSBodyBytes, &set)

		v.jwks.mu.Lock()
		defer v.jwks.mu.Unlock()
		if err != nil {
			return nil, unavailable(fmt.Errorf("fetching JWKS: %w", err))
		}
		v.jwks.keys = parseKeys(set)
		v.jwks.fetchedAt = v.now()
		return v.jwks.keys, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]jwksKey), nil
}

// parseKeys keeps the RSA and EC signature keys, one bad entry never
// discarding the rest. "oct" keys are dropped outright: a symmetric key in a
// public JWKS has no business verifying anything, and it is the key type an
// algorithm-confusion attack needs.
func parseKeys(set jwkset.JWKSMarshal) []jwksKey {
	var out []jwksKey
	for _, m := range set.Keys {
		if m.KTY != jwkset.KtyRSA && m.KTY != jwkset.KtyEC {
			continue
		}
		if m.USE != "" && m.USE != jwkset.UseSig {
			continue
		}
		j, err := jwkset.NewJWKFromMarshal(m, jwkset.JWKMarshalOptions{}, jwkset.JWKValidateOptions{SkipAll: true})
		if err != nil {
			continue
		}
		key := j.Key()
		if pk, ok := key.(interface{ Public() crypto.PublicKey }); ok {
			key = pk.Public()
		}
		out = append(out, jwksKey{kid: m.KID, alg: string(m.ALG), key: key})
	}
	return out
}

// matchKey selects by kid; a token without a kid matches only when the
// provider publishes exactly one key suitable for the token's algorithm.
func matchKey(keys []jwksKey, kid, alg string) (jwksKey, bool) {
	var found []jwksKey
	for _, k := range keys {
		if kid != "" && k.kid != kid {
			continue
		}
		if k.alg != "" && k.alg != alg {
			continue
		}
		if kid == "" && !keySuitsAlg(k.key, alg) {
			continue
		}
		found = append(found, k)
	}
	if len(found) == 1 {
		return found[0], true
	}
	return jwksKey{}, false
}

func keySuitsAlg(key any, alg string) bool {
	switch key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		return strings.HasPrefix(alg, "ES")
	}
	return false
}
