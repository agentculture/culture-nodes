package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PrincipalKind identifies the Cloudflare Access credential shape.
type PrincipalKind string

const (
	// PrincipalInteractive is a person authenticated by an Access identity provider.
	PrincipalInteractive PrincipalKind = "interactive"
	// PrincipalServiceToken is a Cloudflare Access service token.
	PrincipalServiceToken PrincipalKind = "service_token"
)

// Principal is the identity asserted by a verified Cloudflare Access JWT.
// Email is display data only and is never part of the binding key.
type Principal struct {
	Subject    string
	Email      string
	CommonName string
	Kind       PrincipalKind
}

// BindingKey returns the stable provider and subject used to bind this
// principal to an actor.
func (p Principal) BindingKey() (provider, subject string) {
	if p.Kind == PrincipalServiceToken {
		return "cloudflare-service-token", p.CommonName
	}
	return "cloudflare-access", p.Subject
}

// VerificationError is a classified JWT refusal. Reason is safe to use in
// metrics and logs; the error deliberately carries no assertion material.
type VerificationError struct {
	Reason string
}

func (e *VerificationError) Error() string {
	return "access token verification failed: " + e.Reason
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithHTTPClient sets the client used to retrieve the Access JWKS.
func WithHTTPClient(client *http.Client) Option {
	return func(v *Verifier) {
		if client != nil {
			v.client = client
		}
	}
}

// WithJWKSURL overrides the Cloudflare certificate endpoint. It is intended
// for tests and deployments that proxy the standard endpoint.
func WithJWKSURL(endpoint string) Option {
	return func(v *Verifier) { v.jwksURL = endpoint }
}

// WithRefetchWindow sets the minimum interval between forced kid-miss JWKS
// refreshes.
func WithRefetchWindow(window time.Duration) Option {
	return func(v *Verifier) {
		if window >= 0 {
			v.refetchWindow = window
		}
	}
}

// WithClock sets the verifier clock. It is intended for deterministic tests.
func WithClock(now func() time.Time) Option {
	return func(v *Verifier) {
		if now != nil {
			v.now = now
		}
	}
}

// Verifier verifies Cloudflare Access RS256 JWTs and caches their signing
// keys by kid.
type Verifier struct {
	TeamDomain string
	Audience   string

	client        *http.Client
	jwksURL       string
	refetchWindow time.Duration
	now           func() time.Time

	mu                sync.Mutex
	keys              map[string]*rsa.PublicKey
	lastForcedRefresh time.Time
}

// New constructs a Cloudflare Access verifier pinned to teamDomain and
// audience.
func New(teamDomain, audience string, opts ...Option) *Verifier {
	domain := strings.TrimSuffix(strings.TrimSpace(teamDomain), "/")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	v := &Verifier{
		TeamDomain:    domain,
		Audience:      audience,
		client:        http.DefaultClient,
		jwksURL:       "https://" + domain + "/cdn-cgi/access/certs",
		refetchWindow: time.Minute,
		now:           time.Now,
		keys:          make(map[string]*rsa.PublicKey),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(v)
		}
	}
	return v
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

type jwtClaims struct {
	Audience   json.RawMessage `json:"aud"`
	Issuer     string          `json:"iss"`
	Expires    json.Number     `json:"exp"`
	NotBefore  json.Number     `json:"nbf"`
	IssuedAt   json.Number     `json:"iat"`
	Subject    string          `json:"sub"`
	Email      string          `json:"email"`
	CommonName string          `json:"common_name"`
}

// Verify verifies token and returns its interactive or service-token
// principal.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Principal{}, refusal("malformed")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, refusal("malformed")
	}
	var header jwtHeader
	if decodeJSON(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return Principal{}, refusal("malformed")
	}

	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return Principal{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, refusal("malformed")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return Principal{}, refusal("bad_signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, refusal("malformed")
	}
	var claims jwtClaims
	if decodeJSON(payload, &claims) != nil || claims.Subject == "" {
		return Principal{}, refusal("malformed")
	}
	if claims.Issuer != "https://"+v.TeamDomain {
		return Principal{}, refusal("bad_issuer")
	}
	audiences, ok := parseAudience(claims.Audience)
	if !ok {
		return Principal{}, refusal("malformed")
	}
	if !contains(audiences, v.Audience) {
		return Principal{}, refusal("bad_audience")
	}
	expires, err := claims.Expires.Int64()
	if err != nil {
		return Principal{}, refusal("malformed")
	}
	notBefore, err := claims.NotBefore.Int64()
	if err != nil {
		return Principal{}, refusal("malformed")
	}
	now := v.now().Unix()
	if now >= expires {
		return Principal{}, refusal("expired")
	}
	if now < notBefore {
		return Principal{}, refusal("not_yet_valid")
	}

	principal := Principal{Subject: claims.Subject, Email: claims.Email}
	switch {
	case claims.Email != "":
		principal.Kind = PrincipalInteractive
	case claims.CommonName != "":
		principal.Kind = PrincipalServiceToken
		principal.CommonName = claims.CommonName
	default:
		return Principal{}, refusal("malformed")
	}
	return principal, nil
}

func refusal(reason string) *VerificationError {
	return &VerificationError{Reason: reason}
}

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func parseAudience(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, single != ""
	}
	var multiple []string
	if json.Unmarshal(raw, &multiple) != nil || len(multiple) == 0 {
		return nil, false
	}
	return multiple, true
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if key := v.keys[kid]; key != nil {
		return key, nil
	}
	if len(v.keys) == 0 {
		if err := v.fetchKeys(ctx); err != nil {
			return nil, refusal("unknown_kid")
		}
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}

	now := v.now()
	if v.lastForcedRefresh.IsZero() || now.Sub(v.lastForcedRefresh) >= v.refetchWindow {
		v.lastForcedRefresh = now
		if err := v.fetchKeys(ctx); err != nil {
			return nil, refusal("unknown_kid")
		}
	}
	if key := v.keys[kid]; key != nil {
		return key, nil
	}
	return nil, refusal("unknown_kid")
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
}

func (v *Verifier) fetchKeys(ctx context.Context) error {
	endpoint, err := url.Parse(v.jwksURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return fmt.Errorf("invalid JWKS endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS response status %d", response.StatusCode)
	}
	var document jwksDocument
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, encoded := range document.Keys {
		if encoded.KeyID == "" || encoded.KeyType != "RSA" || encoded.Algorithm != "RS256" || encoded.Use != "sig" {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(encoded.Modulus)
		if err != nil || len(modulus) == 0 {
			continue
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(encoded.Exponent)
		if err != nil || len(exponentBytes) == 0 {
			continue
		}
		exponentBig := new(big.Int).SetBytes(exponentBytes)
		if !exponentBig.IsInt64() || exponentBig.Sign() <= 0 || exponentBig.Int64() > int64(^uint(0)>>1) {
			continue
		}
		keys[encoded.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponentBig.Int64())}
	}
	if len(keys) == 0 {
		return fmt.Errorf("JWKS contains no usable keys")
	}
	v.keys = keys
	return nil
}
