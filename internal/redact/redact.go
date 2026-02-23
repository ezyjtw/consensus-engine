package redact

import "net/url"

// RedisAddr sanitizes a Redis address string by removing any embedded password.
// Handles both plain "host:port" and URL-style "redis[s]://user:pass@host:port".
func RedisAddr(addr string) string {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return addr // plain host:port, nothing to redact
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
