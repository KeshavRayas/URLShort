package link

import (
	"net"
	"net/url"
	"strings"
)

const maxTargetURLLength = 2048

// NormalizeTarget validates a destination and returns a canonical form.
//
// The allowlist is deliberately narrow: http and https only, a real host, no
// embedded credentials. Private and loopback hosts are rejected unless
// allowPrivate is set, because a public shortener that redirects to internal
// addresses is an abuse vector even though this service never fetches the URL.
func NormalizeTarget(raw, baseHost string, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalid("target_url", "is required")
	}
	if len(raw) > maxTargetURLLength {
		return "", invalid("target_url", "must be at most 2048 characters")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid("target_url", "is not a valid URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", invalid("target_url", "must use http or https")
	}
	if u.Host == "" {
		return "", invalid("target_url", "must include a host")
	}
	if u.User != nil {
		return "", invalid("target_url", "must not include credentials")
	}

	host := strings.ToLower(u.Hostname())
	if baseHost != "" && host == strings.ToLower(baseHost) {
		return "", invalid("target_url", "must not point at the shortener itself")
	}
	if !allowPrivate && isPrivateHost(host) {
		return "", invalid("target_url", "must not point at a private or loopback address")
	}

	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
