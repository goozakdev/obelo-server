package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// certReloader owns the operator-supplied certificate on disk (ADR-0041,
// OBELO_TLS_MODE=files) and hands the current one to every TLS handshake,
// re-reading the files after they change.
//
// It exists instead of http.Server.ListenAndServeTLS(certFile, keyFile), which
// reads the PEM files exactly once at startup. A certificate is the one piece of
// this server's configuration that stops working on a date nobody chose: certbot
// — or a Makefile, or a scheduled job — rewrites those two files every couple of
// months, and a process still holding the boot-time copy goes on presenting the
// superseded chain until somebody restarts it. Nothing reports that. It is
// invisible right up to the expiry date, at which point every client in the
// household stops trusting the server within the same hour, and the last change
// anyone made to the box was weeks earlier.
//
// The freshness check is a stat on the handshake path, rate-limited by
// checkEvery, rather than a background ticker. A ticker is a goroutine that runs
// forever — and has to be stopped at shutdown, and keeps running on a server
// nobody is connecting to — to watch a file that changes about six times a year.
// Statting costs a couple of microseconds and only happens when someone actually
// connects, which is exactly when a stale certificate would matter. checkEvery
// bounds the cost under a handshake burst (an HLS client opening several
// connections at once must not stat twice per segment) and is also the upper
// bound on how stale a freshly renewed certificate can be — for a chain with
// weeks left to run, an unobservable number.
type certReloader struct {
	certFile, keyFile string

	// checkEvery is the minimum interval between freshness stats. Zero means
	// "check on every handshake", which is what the tests want and what nothing
	// in production should ask for.
	checkEvery time.Duration

	// mu guards every field below. A plain Mutex rather than an RWMutex because
	// the fast path WRITES (lastChecked) as often as it reads, so a read lock
	// would buy nothing and cost a second lock acquisition on the slow path.
	mu          sync.Mutex
	cert        *tls.Certificate
	stamp       fileStamp
	lastChecked time.Time
}

// defaultCertCheckInterval is how often a running server re-stats the
// certificate files. Seconds, not minutes: this is only the window between a
// renewal completing and the new chain being served, and a few seconds of it is
// free, whereas an operator watching a renewal and seeing no change for five
// minutes will conclude reloading is broken and restart the server — which
// teaches them the reload does not work, which is the belief this whole
// mechanism exists to prevent.
const defaultCertCheckInterval = 5 * time.Second

// fileStamp is the cheap "did these change?" signal: modification time and size
// of both files. Deliberately not a hash — hashing a certificate on the
// handshake path to detect a change that happens six times a year is work for
// nothing, and every renewal tool writes a new file, which moves the mtime.
type fileStamp struct {
	certModTime time.Time
	certSize    int64
	keyModTime  time.Time
	keySize     int64
}

// newCertReloader loads the certificate once, up front, and fails if it cannot.
// A bad path or a mismatched pair must stop the boot (config.Validate says the
// same thing first, and for the same reason): the operator asked for TLS, so
// starting without it would hand them a plain-HTTP server they believe is
// encrypted.
func newCertReloader(certFile, keyFile string, checkEvery time.Duration) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, checkEvery: checkEvery}
	cert, stamp, err := loadKeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS certificate: %w", err)
	}
	r.cert, r.stamp, r.lastChecked = cert, stamp, time.Now()
	warnIfKeyIsExposed(keyFile)
	log.Printf("obelo: TLS certificate loaded from %s (%s)", certFile, describeCert(cert))
	return r, nil
}

// GetCertificate is the tls.Config.GetCertificate hook. It answers every
// handshake with the newest certificate that has successfully loaded, checking
// the files for changes at most once per checkEvery.
//
// It never returns an error while it holds a certificate: refusing a handshake
// because the files are mid-rewrite would turn a renewal into an outage.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if now := time.Now(); now.Sub(r.lastChecked) >= r.checkEvery {
		r.lastChecked = now
		r.refreshLocked()
	}
	return r.cert, nil
}

// refreshLocked re-reads the files if their stamp changed, keeping the last good
// certificate if anything goes wrong. Caller holds r.mu.
//
// KEEPING THE OLD CERTIFICATE ON FAILURE IS THE POINT, not a convenience. A
// renewal is not atomic on every setup: for a moment the certificate file can be
// half-written, or the new leaf can be in place before its key is. Serving the
// previous chain through that window is correct — it is still valid, which is
// precisely why the renewal was not urgent — whereas failing the handshake would
// take the listener down for a few milliseconds of file I/O, during a routine
// maintenance operation, on the one deployment shape (a household port-forward)
// where nobody is watching a dashboard.
func (r *certReloader) refreshLocked() {
	stamp, err := statPair(r.certFile, r.keyFile)
	if err != nil {
		// A file that has vanished reads the same as one being replaced. Say so and
		// keep serving; if it really is gone, this repeats every checkEvery.
		log.Printf("obelo: WARNING: cannot stat the TLS certificate files (%v) — still serving the certificate loaded earlier (%s)", err, describeCert(r.cert))
		return
	}
	if stamp == r.stamp {
		return
	}
	cert, stamp, err := loadKeyPair(r.certFile, r.keyFile)
	if err != nil {
		// Note what is NOT updated: r.stamp keeps the old value, so the next check
		// tries again rather than treating this broken content as "the current
		// state of the files". That is what makes a half-written file self-heal a
		// few seconds later without anyone intervening.
		log.Printf("obelo: WARNING: TLS certificate %s changed but did not load (%v) — still serving the previous certificate (%s)", r.certFile, err, describeCert(r.cert))
		return
	}
	r.cert, r.stamp = cert, stamp
	warnIfKeyIsExposed(r.keyFile)
	log.Printf("obelo: reloaded TLS certificate from %s without a restart (%s)", r.certFile, describeCert(cert))
}

// loadKeyPair reads the PEM pair and the stamp that was current when it was
// read. The stamp is taken BEFORE the read, deliberately: if the files change
// while we are reading them we would rather record the older stamp and re-read
// next time than record the newer one and never notice we loaded a torn copy.
func loadKeyPair(certFile, keyFile string) (*tls.Certificate, fileStamp, error) {
	stamp, err := statPair(certFile, keyFile)
	if err != nil {
		return nil, fileStamp{}, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fileStamp{}, err
	}
	return &cert, stamp, nil
}

// statPair collects the modification time and size of both files.
func statPair(certFile, keyFile string) (fileStamp, error) {
	certInfo, err := os.Stat(certFile)
	if err != nil {
		return fileStamp{}, err
	}
	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{
		certModTime: certInfo.ModTime(),
		certSize:    certInfo.Size(),
		keyModTime:  keyInfo.ModTime(),
		keySize:     keyInfo.Size(),
	}, nil
}

// describeCert renders the leaf's subject and expiry for the log. Those two
// facts answer the two questions an operator has when a renewal is in doubt —
// "is it the right certificate" and "how long have I got" — without a trip to
// openssl on a box that may not have it installed.
func describeCert(cert *tls.Certificate) string {
	leaf := cert.Leaf
	if leaf == nil && len(cert.Certificate) > 0 {
		// crypto/tls has populated Leaf since Go 1.23; parse anyway so this cannot
		// silently degrade to "unknown" if that ever changes or a caller builds a
		// Certificate by hand.
		if parsed, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			leaf = parsed
		}
	}
	if leaf == nil {
		return "unparseable leaf"
	}
	return fmt.Sprintf("subject %q, expires %s", leaf.Subject.CommonName, leaf.NotAfter.UTC().Format(time.RFC3339))
}

// warnIfKeyIsExposed logs when the private key is readable by anyone but its
// owner. It is a warning, never a refusal: the file may be group-readable on
// purpose (a certbot deploy hook and a service account sharing a group is a
// normal arrangement), and refusing to boot over it would be this server
// overruling the operator's own filesystem. But a world-readable key is a
// mistake nobody discovers on their own — everything works — so it is worth one
// line in the log at the moment the file is read.
func warnIfKeyIsExposed(keyFile string) {
	info, err := os.Stat(keyFile)
	if err != nil {
		return // The caller is already reporting whatever went wrong here.
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Printf("obelo: WARNING: TLS private key %s is mode %04o — readable beyond its owner; anyone who can read it can impersonate this server. Consider chmod 600.", keyFile, perm)
	}
}
