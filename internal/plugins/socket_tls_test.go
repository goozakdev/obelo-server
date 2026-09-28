package plugins

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// TestTheSocketTLSFloorIsTheHosts: the host's TLS configuration names its own
// floor, TLS 1.2, with and without an operator's CA — not the Go runtime's
// default, which a GODEBUG setting or a toolchain change could move. The floor
// cannot be reached end to end: this toolchain's client already refuses below
// TLS 1.2 whatever the configuration says.
func TestTheSocketTLSFloorIsTheHosts(t *testing.T) {
	parallel(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Obelo Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	for _, trusted := range []string{"", ca} {
		cfg, ok := socketTLSConfig(socketSettings{host: "ldap.example.test", trustedCA: trusted})
		if !ok {
			t.Fatal("socketTLSConfig refused the CA")
		}
		if cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != "ldap.example.test" {
			t.Fatalf("config has MinVersion %#x and ServerName %q, want TLS 1.2 and the operator's host",
				cfg.MinVersion, cfg.ServerName)
		}
	}
}
