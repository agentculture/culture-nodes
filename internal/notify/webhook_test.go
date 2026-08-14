package notify_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// envPrimary/envFallback duplicate the (unexported) constants ResolveWebhook
// reads, deliberately spelled out here rather than imported: a test that
// referenced notify's private constant could pass even if ResolveWebhook
// silently started reading a different variable name than the spec's
// CULTURE_NODES_WEBHOOK_URL / DISCORD_WEBHOOK_URL chain names. Writing the
// literal strings pins the actual contract.
const (
	envPrimary  = "CULTURE_NODES_WEBHOOK_URL"
	envFallback = "DISCORD_WEBHOOK_URL"
)

// fastEnough bounds how long a Post call that must not touch the network is
// allowed to take. postTimeout is 5s; a call that actually dialed anything
// (even a black-holed address) would not return this fast, so a duration
// under this ceiling is evidence no network attempt happened.
const fastEnough = 500 * time.Millisecond

func TestResolveWebhookDisabledWhenNeitherSet(t *testing.T) {
	url, enabled := notify.ResolveWebhook()
	if enabled || url != "" {
		t.Fatalf("want disabled state, got url=%q enabled=%v", url, enabled)
	}
}

func TestResolveWebhookPrimaryWins(t *testing.T) {
	t.Setenv(envPrimary, "https://primary.example/api/webhooks/1/abc")
	t.Setenv(envFallback, "https://fallback.example/hook")

	url, enabled := notify.ResolveWebhook()
	if !enabled || url != "https://primary.example/api/webhooks/1/abc" {
		t.Fatalf("want primary url, got url=%q enabled=%v", url, enabled)
	}
}

func TestResolveWebhookFallsBackWhenPrimaryUnset(t *testing.T) {
	t.Setenv(envFallback, "https://fallback.example/hook")

	url, enabled := notify.ResolveWebhook()
	if !enabled || url != "https://fallback.example/hook" {
		t.Fatalf("want fallback url, got url=%q enabled=%v", url, enabled)
	}
}

func TestResolveWebhookBlankPrimaryFallsThroughToFallback(t *testing.T) {
	t.Setenv(envPrimary, "   ")
	t.Setenv(envFallback, "https://fallback.example/hook")

	url, enabled := notify.ResolveWebhook()
	if !enabled || url != "https://fallback.example/hook" {
		t.Fatalf("blank primary should fall through, got url=%q enabled=%v", url, enabled)
	}
}

func TestResolveWebhookBlankBothIsDisabled(t *testing.T) {
	t.Setenv(envPrimary, "  ")
	t.Setenv(envFallback, "\t\t")

	url, enabled := notify.ResolveWebhook()
	if enabled || url != "" {
		t.Fatalf("blank-both should be disabled, got url=%q enabled=%v", url, enabled)
	}
}

func TestResolveWebhookBlankFallbackWithNoPrimaryIsDisabled(t *testing.T) {
	t.Setenv(envFallback, "")

	url, enabled := notify.ResolveWebhook()
	if enabled || url != "" {
		t.Fatalf("want disabled, got url=%q enabled=%v", url, enabled)
	}
}

func TestPostDisabledDoesZeroNetwork(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer server.Close()

	start := time.Now()
	result := notify.Post(context.Background(), "", []byte(`{}`))
	elapsed := time.Since(start)

	if result != notify.Disabled {
		t.Fatalf("want Disabled, got %v", result)
	}
	if hit {
		t.Fatalf("empty url must never reach any server")
	}
	if elapsed > fastEnough {
		t.Fatalf("disabled Post took %v, want well under %v (evidence it dialed something)", elapsed, fastEnough)
	}
}

func TestPostWhitespaceOnlyURLIsDisabled(t *testing.T) {
	result := notify.Post(context.Background(), "   ", []byte(`{}`))
	if result != notify.Disabled {
		t.Fatalf("want Disabled for whitespace-only url, got %v", result)
	}
}

func TestPostSchemeGuardRejectsNonHTTP(t *testing.T) {
	cases := []string{
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ws://example.com/hook",
		"not-a-url-at-all",
	}
	for _, rawURL := range cases {
		t.Run(rawURL, func(t *testing.T) {
			start := time.Now()
			result := notify.Post(context.Background(), rawURL, []byte(`{}`))
			elapsed := time.Since(start)

			if result != notify.Failed {
				t.Fatalf("want Failed for scheme %q, got %v", rawURL, result)
			}
			if elapsed > fastEnough {
				t.Fatalf("scheme-guard rejection of %q took %v, want well under %v (evidence it dialed something)", rawURL, elapsed, fastEnough)
			}
		})
	}
}

func TestPostSucceedsOn2xxAndSendsExpectedRequest(t *testing.T) {
	var gotMethod, gotContentType, gotUserAgent, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotUserAgent = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result := notify.Post(context.Background(), server.URL, []byte(`{"hello":"world"}`))

	if result != notify.Posted {
		t.Fatalf("want Posted, got %v", result)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("want POST, got %q", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("want application/json content-type, got %q", gotContentType)
	}
	if gotUserAgent == "" {
		t.Fatalf("want a non-empty User-Agent")
	}
	if gotBody != `{"hello":"world"}` {
		t.Fatalf("want request body round-tripped, got %q", gotBody)
	}
}

func TestPostNon2xxIsFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	result := notify.Post(context.Background(), server.URL, []byte(`{}`))
	if result != notify.Failed {
		t.Fatalf("want Failed on 500, got %v", result)
	}
}

// TestPostRedirectIsRefusedAndNeverFollowed is the redirect-refusal /
// SSRF-guard test: a validated https(t) URL that answers with a 3xx must
// never cause Post to dial the Location it points to, even when that
// Location is a perfectly reachable http(s) URL. A followed redirect would
// let a compromised or malicious webhook endpoint bounce the POST to an
// arbitrary host past the scheme guard.
func TestPostRedirectIsRefusedAndNeverFollowed(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	result := notify.Post(context.Background(), redirector.URL, []byte(`{}`))

	if result != notify.Failed {
		t.Fatalf("want Failed on an unfollowed 3xx, got %v", result)
	}
	if targetHit {
		t.Fatalf("redirect target must never be reached — Post must not follow redirects")
	}
}

func TestPostNeverPanicsOnGarbageInput(t *testing.T) {
	cases := []string{
		"",
		" ",
		"http://",
		"https://",
		"http://[::1",
		"\x00\x01\x02",
		"http://user:pass@host:notaport/path",
	}
	for _, rawURL := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Post panicked on %q: %v", rawURL, r)
				}
			}()
			notify.Post(context.Background(), rawURL, []byte(`{}`))
		}()
	}
}

func TestPostContextAlreadyCanceledIsFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := notify.Post(ctx, server.URL, []byte(`{}`))
	if result != notify.Failed {
		t.Fatalf("want Failed on an already-canceled context, got %v", result)
	}
}

func TestIsHTTPURL(t *testing.T) {
	// A Discord webhook URL embeds its own token, so plain http to a real
	// host would put the credential on the wire in cleartext. https is
	// required everywhere except loopback, which the hermetic httptest
	// pattern needs and where nothing leaves the machine.
	cases := map[string]bool{
		"https://example.com":        true,
		"HTTPS://EXAMPLE.COM":        true,
		"http://example.com":         false,
		"http://127.0.0.1:8080/hook": true,
		"http://[::1]:8080/hook":     true,
		"http://localhost:8080/hook": true,
		"http://LOCALHOST:8080/hook": true,
		"ftp://example.com":          false,
		"":                           false,
		"a-schemeless-relative-path": false,
	}
	for rawURL, want := range cases {
		if got := notify.IsHTTPURL(rawURL); got != want {
			t.Errorf("IsHTTPURL(%q) = %v, want %v", rawURL, got, want)
		}
	}
}

func TestIsHTTPURLDoesNotResolveHostnamesToAuthorizeCleartext(t *testing.T) {
	// Only the literal name "localhost" and literal loopback IPs are
	// exempt. Resolving a hostname here would let a name that points at
	// 127.0.0.1 today authorize cleartext, and repoint tomorrow.
	for _, rawURL := range []string{
		"http://localhost.evil.example/hook",
		"http://127.0.0.1.evil.example/hook",
		"http://not-localhost/hook",
	} {
		if notify.IsHTTPURL(rawURL) {
			t.Errorf("IsHTTPURL(%q) = true; a hostname that is not literally loopback must not authorize plain http", rawURL)
		}
	}
}

func TestIsDiscordURL(t *testing.T) {
	cases := map[string]bool{
		"https://discord.com/api/webhooks/123/abc":        true,
		"https://discordapp.com/api/webhooks/123/abc":     true,
		"https://ptb.discord.com/api/webhooks/123/abc":    true,
		"https://canary.discord.com/api/webhooks/123/abc": true,
		"https://discord.com/not-a-webhook":               false,
		"https://evil.example/api/webhooks/123/abc":       false,
		"https://example.com/hook":                        false,
		"not a url\x00":                                   false,
	}
	for rawURL, want := range cases {
		if got := notify.IsDiscordURL(rawURL); got != want {
			t.Errorf("IsDiscordURL(%q) = %v, want %v", rawURL, got, want)
		}
	}
}
