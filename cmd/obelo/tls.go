package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/marioquake/obelo-server/internal/config"
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

// --- ACME (OBELO_TLS_MODE=acme, ADR-0041 phase 2) ---------------------------

// acmeCertificates owns the autocert.Manager for OBELO_TLS_MODE=acme and wraps
// its GetCertificate hook with the logging an unattended household server needs.
//
// The wrapper exists because the two things an operator has to know — "TLS is
// working now" and "TLS is not working, and here is the short list of reasons" —
// are otherwise invisible. autocert returns its errors to the TLS stack, which
// hands them to net/http, which prints "http: TLS handshake error from
// 203.0.113.9: acme/autocert: …": true, unattributed, and indistinguishable from
// the noise a port-forwarded box gets all day.
//
// THIS TYPE MUST NEVER TURN A CERTIFICATE FAILURE INTO A BOOT FAILURE. That is
// the whole posture of `acme` mode and the exact opposite of `files` mode. A CA
// outage, DNS that has not propagated, a port-forward the household has not set
// up yet, or a rate limit are the world being unavailable — none of them is the
// operator's mistake, and ADR-0041 says plainly that refusing to serve the LAN
// over one would be a bad trade. So: log, keep the plain-HTTP listener serving,
// and let autocert try again on the next handshake. config.validateTLS carries
// the same reasoning from the other side.
type acmeCertificates struct {
	manager *autocert.Manager

	// listenAddrs are quoted back in the failure message. An operator reading
	// "no certificate yet" needs to be told, in the same breath, that the LAN
	// listener is fine and which port the router has to forward.
	plainAddr string
	tlsAddr   string
	directory string

	// complainEvery bounds how often a failure is logged. This is not tidiness.
	// The host policy rejects every unknown SNI, and an internet-facing port gets
	// those continuously from scanners — one line each would bury the one failure
	// that is about the operator's own domain under thousands that are not.
	complainEvery time.Duration

	mu         sync.Mutex
	lastGripe  time.Time
	suppressed int
	announced  map[string]bool
}

// defaultACMEComplaintInterval is the floor between two logged certificate
// failures. A minute is short enough that an operator restarting the server and
// reloading a browser sees the reason immediately, and long enough that a
// scanner sweeping SNI names cannot fill the disk.
const defaultACMEComplaintInterval = time.Minute

// newACMECertificates builds the autocert manager from the config and prepares
// its cache directory. It returns no error, deliberately — see the type comment.
func newACMECertificates(cfg config.Config) *acmeCertificates {
	cacheDir := cfg.ACMECacheDir()

	// 0700 because this directory holds the ACME account key and every issued
	// certificate's private key. autocert's DirCache would create it itself (also
	// 0700, on first Put), but only lazily: creating it here means the permissions
	// are right and visible from the first boot, before anything sensitive lands
	// in it, and it gives the check below something to look at.
	//
	// A failure is a warning rather than a refusal, and rather than skipping the
	// listener. The data directory has already been proven writable by
	// config.EnsureDataDir, so getting here at all means something unusual and
	// probably transient; DirCache retries the same MkdirAll on every Put, so a
	// problem that clears fixes itself. What must not happen is a boot failure
	// (that is `files` mode's posture, not this one) or a running server with no
	// cache at all, which would re-issue on every restart and walk straight into
	// the CA's duplicate-certificate limit.
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		log.Printf("obelo: WARNING: cannot create the ACME cache directory %s (%v) — certificates and the ACME account key have nowhere durable to live, so this server may re-issue on every restart and hit the CA's rate limit. HTTPS will still be attempted.", cacheDir, err)
	} else {
		warnIfACMECacheIsExposed(cacheDir)
	}

	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(cacheDir),

		// NEVER nil. With no host policy autocert obtains a certificate for
		// whatever name a client puts in its SNI, which on a port-forwarded box
		// means strangers' host names, the account's rate limit spent on domains
		// this server has no business with, and our IP appearing in somebody else's
		// certificate transparency log entry. config.validateACME refuses to boot
		// with an empty list so this cannot silently become HostWhitelist() — which
		// would be a policy that rejects everything, not one that allows it, but is
		// still not a state worth reaching by accident.
		HostPolicy: autocert.HostWhitelist(cfg.TLSDomains...),

		Email: cfg.ACMEEmail,

		// The directory URL is always explicit, even when it is the default, so the
		// boot log below can name the CA that is about to be talked to. A staging
		// URL here is the difference between debugging a port-forward calmly and
		// being locked out by the production rate limit after five attempts.
		Client: &acme.Client{DirectoryURL: cfg.ACMEDirectoryURL},
	}

	log.Printf("obelo: ACME enabled for %v via %s (cache: %s) — certificates are obtained on the first HTTPS handshake for a listed name, over TLS-ALPN-01 on %s; port 80 is not used and does not need forwarding",
		cfg.TLSDomains, cfg.ACMEDirectoryURL, cacheDir, cfg.TLSListenAddr)

	return &acmeCertificates{
		manager:       m,
		plainAddr:     cfg.ListenAddr,
		tlsAddr:       cfg.TLSListenAddr,
		directory:     cfg.ACMEDirectoryURL,
		complainEvery: defaultACMEComplaintInterval,
		announced:     map[string]bool{},
	}
}

// GetCertificate is the tls.Config.GetCertificate hook for `acme` mode.
//
// Nothing is obtained ahead of time, and that is a decision rather than an
// omission. Issuance is driven by the first handshake for a listed name because
// (a) the CA validates TLS-ALPN-01 by connecting BACK to this listener, so an
// attempt made before the listener is accepting fails for a reason that has
// nothing to do with the operator's setup, and (b) a boot-time attempt on a box
// whose DNS is not pointed yet spends one of the CA's five failed authorizations
// per hostname per hour — on every restart, which is exactly when an operator is
// restarting most. Failing toward "did not talk to the CA" is the cheaper
// mistake. autocert caches what it obtains and renews it on its own timer, so the
// cost of laziness is one slow handshake per certificate per lifetime.
func (a *acmeCertificates) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := a.manager.GetCertificate(hello)

	// The CA validating our challenge is not a user, and its handshake is not
	// news: it presents a throwaway certificate carrying the challenge response
	// and hangs up. Logging it as "a certificate is ready" would be wrong, and
	// logging its failures alongside real ones would be noise.
	if isACMEChallengeHandshake(hello) {
		return cert, err
	}

	if err != nil {
		a.complain(hello.ServerName, err)
		return nil, err
	}
	a.announce(hello.ServerName, cert)
	return cert, nil
}

// isACMEChallengeHandshake reports whether this handshake is a CA verifying a
// TLS-ALPN-01 challenge. The test matches autocert's own (a client offering
// acme-tls/1 and nothing else); no real client ever does that, because a real
// client wants to speak HTTP afterwards.
func isACMEChallengeHandshake(hello *tls.ClientHelloInfo) bool {
	return len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto
}

// announce logs the first successful certificate for each name. Once per name,
// not once per handshake: this is the line an operator greps for to confirm the
// thing worked, and it is worthless if it repeats.
func (a *acmeCertificates) announce(name string, cert *tls.Certificate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.announced[name] {
		return
	}
	a.announced[name] = true
	log.Printf("obelo: HTTPS certificate ready for %q from %s (%s)", name, a.directory, describeCert(cert))
}

// complain reports a certificate that could not be produced, rate-limited to one
// line per complainEvery with a count of what was swallowed.
//
// The message is long on purpose. It arrives on a box nobody is watching, hours
// after the operator changed something, and its job is to answer "what do I go
// and look at" without a second round trip — including the part they will not
// otherwise believe, which is that the server is still running and the LAN is
// unaffected.
func (a *acmeCertificates) complain(name string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	if now.Sub(a.lastGripe) < a.complainEvery {
		a.suppressed++
		return
	}
	a.lastGripe = now

	also := ""
	if a.suppressed > 0 {
		also = fmt.Sprintf(" (%d further certificate failures suppressed since the last message)", a.suppressed)
		a.suppressed = 0
	}
	log.Printf("obelo: WARNING: no HTTPS certificate for %q: %v%s — the server is STILL RUNNING and plain HTTP on %s is unaffected (ADR-0041: a certificate problem costs HTTPS, not the LAN). Check that %q is in OBELO_TLS_DOMAINS, that its public DNS points at this house, that the router forwards 443 to %s, and that %s is reachable. autocert retries on the next handshake.",
		name, err, also, a.plainAddr, name, a.tlsAddr, a.directory)
}

// warnIfACMECacheIsExposed logs when the ACME cache directory is readable or
// traversable beyond its owner. A warning, never a refusal — the same call
// warnIfKeyIsExposed makes below, for the same reason: the operator may have
// arranged group access on purpose, and this server does not get to overrule
// their filesystem. But this directory holds every private key it will ever
// serve, so a mode nobody chose deserves a line in the log.
func warnIfACMECacheIsExposed(dir string) {
	info, err := os.Stat(dir)
	if err != nil {
		return // The caller is already reporting whatever went wrong here.
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Printf("obelo: WARNING: the ACME cache directory %s is mode %04o — readable beyond its owner, and it holds the ACME account key and every issued private key; anyone who can read it can impersonate this server. Consider chmod 700.", dir, perm)
	}
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
