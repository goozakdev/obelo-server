package enrich

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Transient provider failures (ADR-0048). A pass has to answer two different
// questions about a lookup that did not produce a record, and they used to
// collapse into one:
//
//   - "is there a record for this item?" — answered by ErrNoMatch, which files
//     the item as 'unmatched' for an Admin to hand-match;
//   - "did we manage to ask?" — answered here. A DNS failure, a refused
//     connection, a 503 or a rate-limit is not a statement about the item at all.
//     Recording it as a terminal outcome parks a perfectly matchable Title on the
//     attention list until somebody happens to run a full pass.
//
// A provider marks the second kind with ErrTransient and the pass schedules a
// retry instead of parking the row.

// ErrTransient marks a provider failure that describes the CONNECTION or the
// provider's own state rather than the item being looked up — so trying the same
// lookup again later may well succeed. Providers wrap their transport, timeout,
// 5xx and rate-limit failures with it; the pass tests for it with IsTransient.
//
// It is deliberately opt-in: an error carrying no marker is treated as permanent
// and parks the item exactly as before, so a failure mode nobody has classified
// yet degrades to the old, visible behavior rather than to a silent retry loop.
var ErrTransient = errors.New("enrich: transient provider failure")

// IsTransient reports whether err is a provider failure worth retrying.
func IsTransient(err error) bool { return errors.Is(err, ErrTransient) }

// transientError marks an error transient WITHOUT changing what it says. The
// marker is a matching rule, not a message prefix, so an operator reading the log
// still sees "enrich: tmdb /movie/550: status 503" and not that sentence wrapped
// in a second one explaining the retry machinery.
type transientError struct{ err error }

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() error { return e.err }

// Is answers errors.Is(err, ErrTransient) for the marker itself; Unwrap carries
// the chain onward so errors.Is against the underlying cause still works.
func (e transientError) Is(target error) bool { return target == ErrTransient }

// transient marks err transient (nil stays nil).
func transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err}
}

// retryableStatus reports whether an HTTP status from a metadata provider is
// worth asking again. The line is "does this code describe the provider, or does
// it describe our request?":
//
//   - 408 / 429 and every 5xx describe the provider — it is overloaded, throttling
//     us, briefly down, or behind a gateway that is. All retryable.
//   - 400 / 401 / 403 / 404 / 422 describe the request — a malformed query, a key
//     the provider rejects, a record that does not exist. Retrying re-sends the
//     same bad request, so these park and surface to the Admin, who is the only
//     one who can fix a wrong key or a wrong id.
//
// 404 in particular never reaches here as a failure: the providers map it to
// ErrNoMatch, which is a definitive answer about the record.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

// statusError builds the error for a provider's non-2xx response, marking it
// transient when the status describes the provider rather than the request.
// source is the provider's short name ("tmdb"), path the request path.
func statusError(source, path string, code int) error {
	where := source
	if path != "" {
		where = source + " " + path
	}
	err := fmt.Errorf("enrich: %s: status %d", where, code)
	if retryableStatus(code) {
		return transient(err)
	}
	return err
}

// requestError builds the error for a failed HTTP round-trip (DNS, refused
// connection, TLS, timeout, a cancelled context). Always transient: the request
// never reached the provider, so nothing was learned about the item.
//
// A cancelled context is included on purpose. It is how a pass ends at shutdown,
// and marking the Title it was mid-way through as permanently failed meant a
// restart during a large pass silently parked whatever it was holding.
func requestError(source string, err error) error {
	return transient(fmt.Errorf("enrich: %s request: %w", source, err))
}

// decodeError builds the error for a response body that would not parse.
// Transient: a truncated body or an HTML error page from an intercepting proxy is
// a delivery failure, not the provider's answer about the item.
func decodeError(source string, err error) error {
	return transient(fmt.Errorf("enrich: decoding %s response: %w", source, err))
}

// --- Saying WHY a provider refused --------------------------------------------

// maxProviderErrorBody is how much of an error response body is read to quote in
// the log. Enough for any provider's one-line JSON error, small enough that a
// misconfigured host serving an HTML page cannot flood the log.
const maxProviderErrorBody = 512

// ProviderRefusal is what a host said when it turned a request away, in the terms
// the host itself used. It exists because "status 503" is not a diagnosis: it is
// the same three digits whether the operator is rate-limited, blocked, or standing
// in a queue behind everyone else on the internet — and those have opposite
// remedies. An operator reading "503" reasonably concludes they are blocked and
// starts throttling themselves, which fixes nothing when the host is shedding load
// globally.
//
// MusicBrainz labels this precisely and the labels were being discarded:
//
//	x-ratelimit-zone: search-global    which bucket
//	x-ratelimit-who:  search-shed      WHOSE bucket — an IP when it is you,
//	                                   a shed name when it is everyone
//	x-ratelimit-limit / -remaining     how much of it is left
type ProviderRefusal struct {
	Status    int
	Zone      string
	Who       string
	Limit     string
	Remaining string
	// Message is the host's own error text, trimmed.
	Message string
	// Header is the full response header, kept so a caller can read Retry-After
	// without a second capture.
	Header http.Header
}

// readRefusal captures a refusal from a response, consuming a bounded prefix of
// the body. Safe on any response; the caller closes the body as usual.
func readRefusal(resp *http.Response) ProviderRefusal {
	r := ProviderRefusal{
		Status:    resp.StatusCode,
		Zone:      resp.Header.Get("X-RateLimit-Zone"),
		Who:       resp.Header.Get("X-RateLimit-Who"),
		Limit:     resp.Header.Get("X-RateLimit-Limit"),
		Remaining: resp.Header.Get("X-RateLimit-Remaining"),
		Header:    resp.Header,
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBody))
	r.Message = strings.TrimSpace(collapseSpace(string(body)))
	return r
}

// ourQuota reports whether the refusal is about THIS server's own usage — the one
// case where slowing down helps.
//
// The signal is x-ratelimit-who: when the limiter is counting an individual client
// it names that client (an address), and when it is shedding load across everyone
// it names the shed bucket ("search-shed"). An address contains a dot or a colon
// and a shed name does not, which is a crude test — so the host's own wording is
// consulted first, and an ABSENT who is treated as ours, because a refusal we
// cannot attribute is one worth slowing down for.
func (r ProviderRefusal) ourQuota() bool {
	msg := strings.ToLower(r.Message)
	switch {
	case strings.Contains(msg, "exceeding the allowable rate limit"),
		strings.Contains(msg, "rate limit"):
		return true
	case strings.Contains(msg, "currently busy"), strings.Contains(msg, "try again later"):
		return false
	}
	if r.Who == "" {
		return true
	}
	return strings.ContainsAny(r.Who, ".:")
}

// String renders the refusal for a log line: the host's verdict first, then the
// counters, then the one sentence an operator needs — whether this is theirs.
func (r ProviderRefusal) String() string {
	var b strings.Builder
	if r.Message != "" {
		fmt.Fprintf(&b, "%q", r.Message)
	}
	var facts []string
	if r.Zone != "" {
		facts = append(facts, "zone="+r.Zone)
	}
	if r.Who != "" {
		facts = append(facts, "who="+r.Who)
	}
	if r.Remaining != "" || r.Limit != "" {
		facts = append(facts, "quota="+r.Remaining+"/"+r.Limit)
	}
	if len(facts) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(strings.Join(facts, " "))
	}
	if b.Len() > 0 {
		b.WriteString("; ")
	}
	if r.ourQuota() {
		b.WriteString("this is OUR usage — slowing down will help")
	} else {
		b.WriteString("the host is shedding load for everyone — not our rate limit, " +
			"and throttling further will not help")
	}
	return b.String()
}

// collapseSpace flattens whitespace runs so a multi-line HTML error page cannot
// span a dozen log lines.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// refusalError is statusError with the host's own explanation attached, so the
// log says what happened instead of leaving the operator to infer it from three
// digits.
func refusalError(source, path string, r ProviderRefusal) error {
	where := source
	if path != "" {
		where = source + " " + path
	}
	err := fmt.Errorf("enrich: %s: status %d (%s)", where, r.Status, r)
	if retryableStatus(r.Status) {
		return transient(err)
	}
	return err
}

// retryBackoff is the wait before each successive retry of one item, indexed by
// how many consecutive attempts have already failed. It starts short enough that
// a blip clears on the next scan and ends at a daily ceiling, so a provider that
// stays broken costs one call per item per day rather than one per pass.
//
// The waits are a FLOOR, not a schedule: nothing wakes up to run a retry. A pass
// runs when a scan finishes or the sweep ticks, and picks up whatever has come
// due since — so with a sweep interval longer than the entry here, the early
// steps simply never bind.
//
// Its length is store.EnrichRetryEscalateAfter by construction: an item escalates
// to the Admin's attention list exactly when its backoff reaches the ceiling
// (retry_escalation_test.go pins the two together).
var retryBackoff = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	3 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

// retryDelay returns how long to wait after `attempts` consecutive failures
// before trying again. Past the end of the schedule the ceiling repeats forever —
// there is no attempt cap, because "we could not reach the provider" never becomes
// evidence about the item, however many times it is said.
func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(retryBackoff) {
		attempts = len(retryBackoff)
	}
	return retryBackoff[attempts-1]
}
