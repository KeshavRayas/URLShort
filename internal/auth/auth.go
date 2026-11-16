// Package auth maps API credentials to users.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

// User is the authenticated caller attached to a request context.
type User struct {
	ID   uuid.UUID
	Name string
}

// ErrNotFound means the presented credential does not map to an account.
var ErrNotFound = errors.New("auth: user not found")

// Store looks up a user by the hash of their presented API key.
type Store interface {
	UserByAPIKeyHash(ctx context.Context, keyHash string) (User, error)
}

// HashKey derives the stored form of an API key.
//
// Keys are hashed, never encrypted: the service only needs to compare, and a
// database leak should not hand over working credentials. The pepper keeps a
// leaked table from being attacked with an offline dictionary.
func HashKey(pepper, key string) string {
	sum := sha256.Sum256([]byte(pepper + key))
	return hex.EncodeToString(sum[:])
}
