package link

import (
	crand "crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

// codeAlphabet is base62. It is URL-safe without escaping and avoids the
// ambiguous-looking characters of a wider set only at the cost of nothing here;
// base58's excluded glyphs matter more when humans retype codes.
const codeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var codePattern = regexp.MustCompile(`^[0-9A-Za-z]{3,32}$`)

// NewCode returns a cryptographically random base62 string of length n.
//
// Bytes >= 248 are rejected because 248 = 62*4 is the largest multiple of 62
// that fits in a byte; this keeps the modulo mapping unbiased.
func NewCode(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("code length must be positive")
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := crand.Read(buf); err != nil {
			return "", fmt.Errorf("generate code: %w", err)
		}
		for _, b := range buf {
			if b >= 248 {
				continue
			}
			out = append(out, codeAlphabet[b%62])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// ValidCode reports whether s has the shape of a short code. It is checked
// before any cache or database lookup so malformed paths are cheap.
func ValidCode(s string) bool {
	return codePattern.MatchString(s)
}

// reservedCodes are paths the router owns, plus names that would be confusing
// as aliases. Custom aliases may not use them.
var reservedCodes = map[string]struct{}{
	"api": {}, "healthz": {}, "readyz": {}, "metrics": {},
	"admin": {}, "static": {}, "assets": {}, "links": {},
	"favicon.ico": {}, "robots.txt": {},
}

// Reserved reports whether code is a name the service keeps for itself.
func Reserved(code string) bool {
	_, ok := reservedCodes[strings.ToLower(code)]
	return ok
}
