package notify

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// envPrimary and envFallback are the only two places the webhook URL is
// ever read from. Both are environment variables, never a config file or
// a CLI flag — the URL embeds a bearer token, and env-only keeps it out of
// anything that gets committed, logged, or journaled.
const (
	envPrimary  = "CULTURE_NODES_WEBHOOK_URL"
	envFallback = "DISCORD_WEBHOOK_URL"
)

// postTimeout bounds the single POST attempt Post makes. There is no
// retry: a webhook problem must never make a caller wait longer than this
// once, and must never cascade into a second attempt.
const postTimeout = 5 * time.Second

// userAgent identifies this transport in the (non-secret) request headers
// it sends. It names neither the URL nor the run it is notifying about.
const userAgent = "culture-nodes-notify/1"

// discordHosts is the set of hosts a Discord webhook URL can use. Matches
// devex's _DISCORD_HOSTS.
var discordHosts = map[string]bool{
	"discord.com":        true,
	"discordapp.com":     true,
	"ptb.discord.com":    true,
	"canary.discord.com": true,
}

// ResolveWebhook reads the webhook URL from the environment: envPrimary
// (CULTURE_NODES_WEBHOOK_URL) wins when set to a non-blank value, else
// envFallback (DISCORD_WEBHOOK_URL) is tried the same way. A value that is
// empty or all whitespace counts as unset, exactly like a value that was
// never set at all — so `CULTURE_NODES_WEBHOOK_URL=""` in an env file
// falls through to DISCORD_WEBHOOK_URL rather than disabling the webhook
// outright.
//
// enabled is false, and url is "", when neither variable resolves to a
// usable value — the disabled state the rest of this package (and its
// caller) must treat as "make no network call". ResolveWebhook never logs,
// wraps, or otherwise surfaces the URL through an error value; the only
// way to learn it is this direct return.
func ResolveWebhook() (resolvedURL string, enabled bool) {
	if v := strings.TrimSpace(os.Getenv(envPrimary)); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(os.Getenv(envFallback)); v != "" {
		return v, true
	}
	return "", false
}

// PostResult is the outcome of Post. It is deliberately coarse: Post
// never distinguishes *why* a delivery failed (bad scheme, refused
// redirect, non-2xx, timeout, transport error) because none of those
// reasons changes what a fail-open caller does next.
type PostResult string

const (
	// Disabled means url was empty — Post made no network call.
	Disabled PostResult = "disabled"
	// Posted means the request was sent and the response was 2xx.
	Posted PostResult = "posted"
	// Failed covers every other outcome: a non-http(s) scheme, a 3xx
	// redirect (never followed), a non-2xx response, a timeout, or any
	// other transport error.
	Failed PostResult = "failed"
)

// httpClient is shared by every Post call. CheckRedirect returning
// http.ErrUseLastResponse tells net/http to stop at the first redirect
// and hand back the 3xx response as-is instead of following it — Post
// then treats that response like any other non-2xx status: Failed. This
// is the SSRF / scheme-guard bypass a followed redirect would otherwise
// open (a validated https:// URL redirecting to an attacker-controlled
// host or a non-http(s) scheme).
var httpClient = &http.Client{
	Timeout: postTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// IsHTTPURL reports whether Post may send a request to rawURL: https
// anywhere, and plain http ONLY to a loopback host. Malformed URLs report
// false, not an error: a bad URL is exactly the case this guard exists to
// catch.
//
// The https requirement is not generic hygiene. A Discord webhook URL
// *embeds its own credential* — the token is a path segment — so posting one
// over plain http puts the credential, and the message, on the wire in
// cleartext for anything between here and the host. There is no
// authenticated-but-unencrypted mode to fall back to; the URL is the secret.
//
// Loopback keeps its exemption because the hermetic test pattern this package
// is built around (a local httptest server, no network) has no TLS and needs
// none: nothing leaves the machine, so there is nothing to intercept. That
// exemption is what lets the tests exercise the real transport instead of a
// mock of it.
func IsHTTPURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		return isLoopbackHost(parsed.Hostname())
	default:
		return false
	}
}

// isLoopbackHost reports whether host names this machine. A hostname other
// than "localhost" is deliberately NOT resolved — a DNS lookup here would let
// a name that currently resolves to 127.0.0.1 authorize cleartext, and change
// its mind later.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// IsDiscordURL reports whether rawURL is a Discord webhook endpoint:
// a host in discordHosts *and* a path containing "/api/webhooks/". Both
// conditions matter — a bare discord.com link that is not a webhook path
// must not be shaped as one. Matches devex's is_discord_url.
func IsDiscordURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return discordHosts[host] && strings.Contains(parsed.Path, "/api/webhooks/")
}

// Post sends body (already-shaped JSON — see BuildMessage) to rawURL as a
// single bounded POST. It never panics and never returns an error; every
// failure mode collapses into the Failed result so a caller can stay
// fail-open without a type switch on error causes.
//
//   - rawURL == "" (or all whitespace) -> Disabled, with no network call
//     at all — this is the path ResolveWebhook's disabled state takes.
//   - a non-http(s) scheme -> Failed, also with no network call (the
//     scheme guard rejects it before dialing anything).
//   - otherwise: one POST, bounded to postTimeout regardless of what ctx
//     allows, redirects never followed (see httpClient), 2xx -> Posted,
//     anything else (3xx/4xx/5xx, timeout, DNS failure, connection
//     refused, ...) -> Failed.
func Post(ctx context.Context, rawURL string, body []byte) PostResult {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return Disabled
	}
	if !IsHTTPURL(trimmed) {
		return Failed
	}

	boundCtx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(boundCtx, http.MethodPost, trimmed, bytes.NewReader(body))
	if err != nil {
		return Failed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return Failed
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Posted
	}
	return Failed
}
