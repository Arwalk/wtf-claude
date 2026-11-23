// Package api talks to the (undocumented) Claude OAuth usage and profile
// endpoints used by Claude Code's /usage command, and parses their responses
// tolerantly.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultBaseURL is the Anthropic API origin.
	DefaultBaseURL = "https://api.anthropic.com"
	// UsagePath returns rate-limit utilisation for the subscription.
	UsagePath = "/api/oauth/usage"
	// ProfilePath returns account / organisation / plan details.
	ProfilePath = "/api/oauth/profile"
	// BetaHeader is required for OAuth-authenticated requests.
	BetaHeader = "oauth-2025-04-20"

	maxBody = 4 << 20
)

// Client performs authenticated GET requests.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	Token     string
	UserAgent string
}

// HTTPError is returned for non-2xx responses.
type HTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.Status, http.StatusText(e.Status))
}

// Get fetches path and returns the response body.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("anthropic-beta", BetaHeader)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	// Following a redirect would send the token along, even from https to
	// plain http on the same host. The endpoints have no need for them.
	noRedirect := *hc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return body, nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// IsAuthError reports whether err is a 401 (token missing, expired or
// revoked) response.
func IsAuthError(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized
}

// IsAccessError reports whether err is a 401 or 403 response: retrying will
// not help until the user logs in again or fixes access.
func IsAccessError(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden)
}

// RetryAfter returns the server-requested back-off for err, if any.
func RetryAfter(err error) time.Duration {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.RetryAfter
	}
	return 0
}

// Describe turns err into a short, user-facing reason (never containing the
// token or response bodies). Messages are kept short and lead with the
// actionable part because widgets truncate long lines.
func Describe(err error) string {
	if err == nil {
		return ""
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == http.StatusUnauthorized:
			return "login expired · run claude"
		case he.Status == http.StatusForbidden:
			// Missing scope, account restriction, or a proxy/firewall
			// denial: logging in again would not help.
			return "access denied (403)"
		case he.Status == http.StatusTooManyRequests:
			return "rate limited (429)"
		case he.Status >= 300 && he.Status <= 399:
			return fmt.Sprintf("redirect refused (%d)", he.Status)
		case he.Status >= 500:
			return fmt.Sprintf("server error (%d)", he.Status)
		default:
			return fmt.Sprintf("HTTP %d", he.Status)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "request timed out"
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return "network error (DNS)"
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return "network error"
	}
	if errors.Is(err, ErrNotObject) {
		return "unexpected response format"
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		msg = ue.Err.Error()
		// net/http reports a refused proxy CONNECT as the bare status text
		// of the proxy's reply, e.g. "Forbidden".
		if isStatusText(msg) {
			return "proxy refused (" + msg + ")"
		}
	}
	if len(msg) > 60 {
		msg = cutAtRune(msg, 57) + "..."
	}
	return msg
}

func isStatusText(s string) bool {
	for code := 100; code < 600; code++ {
		if t := http.StatusText(code); t != "" && t == s {
			return true
		}
	}
	return false
}

// cutAtRune cuts s to at most n bytes without splitting a UTF-8 character.
func cutAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
