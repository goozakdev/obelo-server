package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The verifier's own cases, beyond the ones the black-box suite drives through
// the Bundled plugin: the algorithms it refuses outright, an EC key, and an aud
// naming several clients.

// ecIssuer serves a discovery document and a one-key EC JWKS.
func ecIssuer(t *testing.T) (*httptest.Server, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		size := 32
		x := make([]byte, size)
		y := make([]byte, size)
		key.X.FillBytes(x)
		key.Y.FillBytes(y)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "ec-1",
			"x": base64.RawURLEncoding.EncodeToString(x), "y": base64.RawURLEncoding.EncodeToString(y),
		}}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, key
}

func segment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func signES256(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signing := segment(t, map[string]string{"alg": "ES256", "kid": "ec-1"}) + "." + segment(t, claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestTheVerifierChecksWhatAnIDTokenClaims(t *testing.T) {
	srv, key := ecIssuer(t)
	good := func() map[string]any {
		return map[string]any{
			"iss": srv.URL, "aud": "client", "sub": "s-1", "nonce": "n-1",
			"exp": time.Now().Add(time.Hour).Unix(), "groups": []string{"family"},
		}
	}

	for _, tc := range []struct {
		name  string
		token func() string
		ok    bool
	}{
		{"an EC-signed token that verifies", func() string { return signES256(t, key, good()) }, true},
		{"several audiences with azp naming the client", func() string {
			c := good()
			c["aud"] = []string{"other", "client"}
			c["azp"] = "client"
			return signES256(t, key, c)
		}, true},
		{"several audiences and no azp", func() string {
			c := good()
			c["aud"] = []string{"other", "client"}
			return signES256(t, key, c)
		}, false},
		{"alg none", func() string {
			return segment(t, map[string]string{"alg": "none"}) + "." + segment(t, good()) + "."
		}, false},
		{"an HMAC signature", func() string {
			signing := segment(t, map[string]string{"alg": "HS256", "kid": "ec-1"}) + "." + segment(t, good())
			mac := hmac.New(sha256.New, []byte(""))
			mac.Write([]byte(signing))
			return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}, false},
		{"no exp", func() string {
			c := good()
			delete(c, "exp")
			return signES256(t, key, c)
		}, false},
		{"no subject", func() string {
			c := good()
			c["sub"] = " "
			return signES256(t, key, c)
		}, false},
		{"nbf in the future", func() string {
			c := good()
			c["nbf"] = time.Now().Add(time.Hour).Unix()
			return signES256(t, key, c)
		}, false},
		{"nbf inside the skew", func() string {
			c := good()
			c["nbf"] = time.Now().Add(10 * time.Second).Unix()
			return signES256(t, key, c)
		}, true},
		{"iat in the future", func() string {
			c := good()
			c["iat"] = time.Now().Add(time.Hour).Unix()
			return signES256(t, key, c)
		}, false},
		{"iat inside the skew", func() string {
			c := good()
			c["iat"] = time.Now().Add(10 * time.Second).Unix()
			return signES256(t, key, c)
		}, true},
		{"a tampered payload", func() string {
			tok := signES256(t, key, good())
			c := good()
			c["sub"] = "s-2"
			parts := strings.Split(tok, ".")
			return parts[0] + "." + segment(t, c) + "." + parts[2]
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIDTokenVerifier()
			claims, err := v.verify(context.Background(), tc.token(), srv.URL+"/", "client", "n-1")
			if (err == nil) != tc.ok {
				t.Fatalf("verify = %+v, %v; want ok=%v", claims, err, tc.ok)
			}
			if tc.ok && (claims.Subject != "s-1" || len(claims.Groups) != 1 || claims.Groups[0] != "family") {
				t.Fatalf("claims = %+v, want s-1 in family", claims)
			}
		})
	}
}

// TestTheClockSkewIsThirtySeconds pins the skew on exp, nbf and iat at its
// boundary, with the verifier's clock held: a second inside it is accepted, a
// second past it refused.
func TestTheClockSkewIsThirtySeconds(t *testing.T) {
	srv, key := ecIssuer(t)
	now := time.Unix(1_900_000_000, 0)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }

	for _, tc := range []struct {
		name  string
		claim string
		at    int64
		ok    bool
	}{
		{"exp 29 s ago", "exp", at(-29 * time.Second), true},
		{"exp 31 s ago", "exp", at(-31 * time.Second), false},
		{"nbf 29 s to come", "nbf", at(29 * time.Second), true},
		{"nbf 31 s to come", "nbf", at(31 * time.Second), false},
		{"iat 29 s to come", "iat", at(29 * time.Second), true},
		{"iat 31 s to come", "iat", at(31 * time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{
				"iss": srv.URL, "aud": "client", "sub": "s-1", "nonce": "n-1",
				"exp": at(time.Hour),
			}
			claims[tc.claim] = tc.at
			v := newIDTokenVerifier()
			v.now = func() time.Time { return now }
			_, err := v.verify(context.Background(), signES256(t, key, claims), srv.URL+"/", "client", "n-1")
			if (err == nil) != tc.ok {
				t.Fatalf("verify with %s at now%+ds = %v; want ok=%v", tc.claim, tc.at-now.Unix(), err, tc.ok)
			}
		})
	}
}

// issuerServing serves a discovery document naming docIssuer (the server's own
// URL when empty) and a JWKS of key with jwk's fields over the defaults.
func issuerServing(t *testing.T, key *ecdsa.PrivateKey, docIssuer string, jwk map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := docIssuer
		if iss == "" {
			iss = srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		x := make([]byte, 32)
		y := make([]byte, 32)
		key.X.FillBytes(x)
		key.Y.FillBytes(y)
		k := map[string]string{
			"kty": "EC", "crv": "P-256", "kid": "ec-1",
			"x": base64.RawURLEncoding.EncodeToString(x), "y": base64.RawURLEncoding.EncodeToString(y),
		}
		for f, v := range jwk {
			k[f] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{k}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// signES256Kid is signES256 with the header naming kid.
func signES256Kid(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signing := segment(t, map[string]string{"alg": "ES256", "kid": kid}) + "." + segment(t, claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestTheVerifierTrustsOnlyTheIssuersOwnKeys: a token whose claims and signature
// are otherwise good is refused when the discovery document names another
// issuer, when its header names a key id the JWKS does not hold (the key that
// signed it sits under another id), and when the key that signed it is
// published for another algorithm.
func TestTheVerifierTrustsOnlyTheIssuersOwnKeys(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		docIssuer string
		jwk       map[string]string
		kid       string
		ok        bool
	}{
		{"the issuer's key, named", "", nil, "ec-1", true},
		{"a discovery document naming another issuer", "https://someone-else.example", nil, "ec-1", false},
		{"a key id the JWKS does not hold", "", nil, "ec-2", false},
		{"a key published for another algorithm", "", map[string]string{"alg": "ES512"}, "ec-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := issuerServing(t, key, tc.docIssuer, tc.jwk)
			tok := signES256Kid(t, key, tc.kid, map[string]any{
				"iss": srv.URL, "aud": "client", "sub": "s-1", "nonce": "n-1",
				"exp": time.Now().Add(time.Hour).Unix(),
			})
			claims, err := newIDTokenVerifier().verify(context.Background(), tok, srv.URL, "client", "n-1")
			if (err == nil) != tc.ok {
				t.Fatalf("verify = %+v, %v; want ok=%v", claims, err, tc.ok)
			}
		})
	}
}
