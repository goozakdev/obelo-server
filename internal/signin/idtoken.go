package signin

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256" // the digests algHash names
	_ "crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// The host's verification of an OpenID Connect ID token (ADR-0063 decision 2).
//
// The keys come from the issuer the OPERATOR typed: its discovery document,
// which must name that same issuer, and the JWKS it points at. Nothing the
// Plugin says reaches this file but the raw token itself — not a key, not an
// endpoint, not a claim — so a Plugin that lies about who somebody is can only
// hand over a token that fails here.
//
// A token is good when its signature verifies under one of the issuer's keys
// with an asymmetric algorithm, and its iss is the issuer, its aud names the
// client id (with azp naming it too when there are several audiences), its nonce
// is the one this server minted for the round trip, its exp has not passed, and
// neither its nbf nor its iat is still to come.

// idTokenClaims is what the host takes from a verified token.
type idTokenClaims struct {
	Subject           string
	PreferredUsername string
	Groups            []string
}

// idTokenLeeway is the clock skew tolerated on exp, nbf and iat.
const idTokenLeeway = 30 * time.Second

// jwksTTL is how long an issuer's keys are trusted before they are fetched
// again; a token naming a key the cache does not hold refetches sooner, but not
// more often than jwksMinRefetch.
const (
	jwksTTL        = 10 * time.Minute
	jwksMinRefetch = 10 * time.Second
)

// maxDocumentBytes bounds a discovery document or a JWKS.
const maxDocumentBytes = 1 << 20

type idTokenVerifier struct {
	client *http.Client
	now    func() time.Time

	mu   sync.Mutex
	keys map[string]issuerKeys // by issuer
}

type issuerKeys struct {
	keys    []jwk
	fetched time.Time
}

func newIDTokenVerifier() *idTokenVerifier {
	return &idTokenVerifier{
		client: safefetch.Client(10 * time.Second),
		now:    time.Now,
		keys:   map[string]issuerKeys{},
	}
}

// verify checks raw against issuer, clientID and nonce and answers its claims.
func (v *idTokenVerifier) verify(ctx context.Context, raw, issuer, clientID, nonce string) (idTokenClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, errors.New("the ID token is not a compact JWS")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return idTokenClaims{}, fmt.Errorf("the ID token's header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idTokenClaims{}, errors.New("the ID token's signature is not base64url")
	}
	signed := []byte(parts[0] + "." + parts[1])

	if err := v.verifySignature(ctx, issuer, header.Alg, header.Kid, signed, sig); err != nil {
		return idTokenClaims{}, err
	}

	var c struct {
		Issuer            string          `json:"iss"`
		Subject           string          `json:"sub"`
		Audience          json.RawMessage `json:"aud"`
		AuthorizedParty   string          `json:"azp"`
		Nonce             string          `json:"nonce"`
		Expiry            *json.Number    `json:"exp"`
		NotBefore         *json.Number    `json:"nbf"`
		IssuedAt          *json.Number    `json:"iat"`
		PreferredUsername string          `json:"preferred_username"`
		Groups            json.RawMessage `json:"groups"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return idTokenClaims{}, fmt.Errorf("the ID token's payload: %w", err)
	}
	if !sameIssuer(c.Issuer, issuer) {
		return idTokenClaims{}, errors.New("the ID token's iss is not the configured issuer")
	}
	aud, err := audiences(c.Audience)
	if err != nil {
		return idTokenClaims{}, err
	}
	if !contains(aud, clientID) || (len(aud) > 1 && c.AuthorizedParty != clientID) {
		return idTokenClaims{}, errors.New("the ID token's aud does not name the configured client id")
	}
	if nonce == "" || c.Nonce != nonce {
		return idTokenClaims{}, errors.New("the ID token's nonce is not the one this sign-in minted")
	}
	if c.Expiry == nil {
		return idTokenClaims{}, errors.New("the ID token has no exp")
	}
	exp, err := c.Expiry.Float64()
	if err != nil {
		return idTokenClaims{}, errors.New("the ID token's exp is not a number")
	}
	if !v.now().Before(time.Unix(int64(exp), 0).Add(idTokenLeeway)) {
		return idTokenClaims{}, errors.New("the ID token has expired")
	}
	if err := v.notYet(c.NotBefore, "nbf"); err != nil {
		return idTokenClaims{}, err
	}
	if err := v.notYet(c.IssuedAt, "iat"); err != nil {
		return idTokenClaims{}, err
	}
	subject := strings.TrimSpace(c.Subject)
	if subject == "" {
		return idTokenClaims{}, errors.New("the ID token names no subject")
	}
	var groups []string
	if len(c.Groups) > 0 && string(c.Groups) != "null" {
		if err := json.Unmarshal(c.Groups, &groups); err != nil {
			return idTokenClaims{}, errors.New("the ID token's groups are not a list of strings")
		}
	}
	return idTokenClaims{
		Subject:           subject,
		PreferredUsername: strings.TrimSpace(c.PreferredUsername),
		Groups:            groups,
	}, nil
}

// notYet refuses a time claim that is still to come, beyond the skew. A claim
// the token does not carry is no refusal: neither nbf nor iat is one an issuer
// must send.
func (v *idTokenVerifier) notYet(claim *json.Number, name string) error {
	if claim == nil {
		return nil
	}
	at, err := claim.Float64()
	if err != nil {
		return fmt.Errorf("the ID token's %s is not a number", name)
	}
	if v.now().Add(idTokenLeeway).Before(time.Unix(int64(at), 0)) {
		return fmt.Errorf("the ID token's %s is still to come", name)
	}
	return nil
}

// verifySignature checks sig over signed with the issuer's key for kid, fetching
// the keys again once when none of the cached ones verifies — which is what an
// issuer rotating its key looks like.
func (v *idTokenVerifier) verifySignature(ctx context.Context, issuer, alg, kid string, signed, sig []byte) error {
	hash, ok := algHash(alg)
	if !ok {
		return fmt.Errorf("the ID token is signed with %q, which this server does not accept", alg)
	}
	for attempt := 0; attempt < 2; attempt++ {
		keys, fresh, err := v.issuerKeys(ctx, issuer, attempt > 0)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if kid != "" && k.Kid != kid {
				continue
			}
			if k.verify(alg, hash, signed, sig) {
				return nil
			}
		}
		if fresh {
			break
		}
	}
	return errors.New("the ID token's signature does not verify against the issuer's keys")
}

// issuerKeys answers the issuer's keys and whether they were fetched just now.
func (v *idTokenVerifier) issuerKeys(ctx context.Context, issuer string, refetch bool) ([]jwk, bool, error) {
	v.mu.Lock()
	cached, ok := v.keys[issuer]
	v.mu.Unlock()
	age := v.now().Sub(cached.fetched)
	if ok && age < jwksTTL && (!refetch || age < jwksMinRefetch) {
		return cached.keys, false, nil
	}
	keys, err := v.fetchKeys(ctx, issuer)
	if err != nil {
		return nil, false, err
	}
	v.mu.Lock()
	v.keys[issuer] = issuerKeys{keys: keys, fetched: v.now()}
	v.mu.Unlock()
	return keys, true, nil
}

// fetchKeys reads the issuer's discovery document and the JWKS it names.
func (v *idTokenVerifier) fetchKeys(ctx context.Context, issuer string) ([]jwk, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, fmt.Errorf("the issuer's discovery document: %w", err)
	}
	if !sameIssuer(doc.Issuer, issuer) {
		return nil, errors.New("the issuer's discovery document names a different issuer")
	}
	if doc.JWKSURI == "" {
		return nil, errors.New("the issuer's discovery document names no jwks_uri")
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, doc.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("the issuer's JWKS: %w", err)
	}
	return set.Keys, nil
}

func (v *idTokenVerifier) getJSON(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxDocumentBytes {
		return errors.New("the document is too large")
	}
	return json.Unmarshal(body, out)
}

// jwk is one key of a JWKS: RSA or EC, and nothing symmetric.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// verify reports whether sig over signed verifies under this key by alg.
func (k jwk) verify(alg string, hash crypto.Hash, signed, sig []byte) bool {
	if k.Use != "" && k.Use != "sig" {
		return false
	}
	if k.Alg != "" && k.Alg != alg {
		return false
	}
	h := hash.New()
	h.Write(signed)
	digest := h.Sum(nil)
	switch {
	case k.Kty == "RSA" && (strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")):
		pub, ok := k.rsaKey()
		if !ok {
			return false
		}
		if strings.HasPrefix(alg, "PS") {
			return rsa.VerifyPSS(pub, hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}
		return rsa.VerifyPKCS1v15(pub, hash, digest, sig) == nil
	case k.Kty == "EC" && strings.HasPrefix(alg, "ES"):
		pub, size, ok := k.ecKey(alg)
		if !ok || len(sig) != 2*size {
			return false
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		return ecdsa.Verify(pub, digest, r, s)
	}
	return false
}

func (k jwk) rsaKey() (*rsa.PublicKey, bool) {
	n, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(n) == 0 {
		return nil, false
	}
	e, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(e) == 0 || len(e) > 4 {
		return nil, false
	}
	exp := int(new(big.Int).SetBytes(e).Int64())
	if exp < 3 {
		return nil, false
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, true
}

func (k jwk) ecKey(alg string) (*ecdsa.PublicKey, int, bool) {
	var curve elliptic.Curve
	switch {
	case alg == "ES256" && k.Crv == "P-256":
		curve = elliptic.P256()
	case alg == "ES384" && k.Crv == "P-384":
		curve = elliptic.P384()
	case alg == "ES512" && k.Crv == "P-521":
		curve = elliptic.P521()
	default:
		return nil, 0, false
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, 0, false
	}
	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, 0, false
	}
	pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !curve.IsOnCurve(pub.X, pub.Y) {
		return nil, 0, false
	}
	return pub, (curve.Params().BitSize + 7) / 8, true
}

// algHash is the digest an accepted algorithm signs over. Only asymmetric
// algorithms are accepted: "none" and every HMAC one are refused, because a
// token either is signed by a key the issuer published or proves nothing.
func algHash(alg string) (crypto.Hash, bool) {
	switch alg {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256, true
	case "RS384", "PS384", "ES384":
		return crypto.SHA384, true
	case "RS512", "PS512", "ES512":
		return crypto.SHA512, true
	}
	return 0, false
}

func decodeSegment(seg string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("not base64url")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return errors.New("not a JSON object")
	}
	return nil
}

// audiences reads aud, which is a string or a list of them.
func audiences(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, errors.New("the ID token's aud is neither a string nor a list of strings")
}

// sameIssuer compares two issuers exactly, but for one trailing slash: the
// operator typing https://auth.example/app and the issuer saying
// https://auth.example/app/ mean the same issuer.
func sameIssuer(a, b string) bool {
	return a != "" && strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func contains(list []string, s string) bool {
	for _, have := range list {
		if have == s && s != "" {
			return true
		}
	}
	return false
}
