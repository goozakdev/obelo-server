package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The fetch helpers every source-shaped plugin would otherwise write for itself:
// build a URL with a query, ask the host, tell the three ways it can fail apart,
// decode JSON.
//
// They are helpers and not a second interface. A plugin that wants the raw
// FetchResponse — because it is downloading a subtitle file, or reading an ETag —
// calls Host.Fetch and is not made to go through here.

// FetchError is a fetch that produced no usable document, with the three facts a
// provider decides on: whether the HOST refused, whether the transport failed,
// and what status the source answered.
//
// The distinction is not decoration. A refusal will be refused again, so the
// plugin should report it and stop; a transport failure or a 5xx is worth a
// retry, and the pass above (ADR-0048) is the thing that retries it. A provider
// that folded all three into "something went wrong" would make every outage look
// like a settled answer, which is the failure ADR-0059 was written after.
type FetchError struct {
	// URL is the target, for the sentence an operator reads.
	URL string
	// Status is what the source answered, 0 when it answered nothing.
	Status int
	// Refused is the host's own words for declining. A plugin must NOT branch on
	// the text: any non-empty value means this server will not do this.
	Refused string
	// Transport is the host having tried and failed.
	Transport string
	// Decode is the answer not being the shape this plugin knows.
	Decode error
}

func (e *FetchError) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Refused != "":
		return fmt.Sprintf("fetching %s was refused by the host: %s", e.URL, e.Refused)
	case e.Transport != "":
		return fmt.Sprintf("fetching %s failed: %s", e.URL, e.Transport)
	case e.Decode != nil:
		return fmt.Sprintf("the answer from %s is not the shape this plugin knows: %v", e.URL, e.Decode)
	default:
		return fmt.Sprintf("%s answered %d", e.URL, e.Status)
	}
}

// Unwrap exposes a decode failure to errors.Is/As; the other three carry no
// wrapped error because they never had one.
func (e *FetchError) Unwrap() error { return e.Decode }

// IsRefusal reports the host declining. Nothing about retrying it will change
// the answer.
func (e *FetchError) IsRefusal() bool { return e != nil && e.Refused != "" }

// IsTransient reports a failure worth trying again later: the transport, or a
// source answering 5xx or 429. A 4xx that is not 429 is the source saying no, and
// asking again with the same request gets the same no.
func (e *FetchError) IsTransient() bool {
	if e == nil || e.Refused != "" {
		return false
	}
	return e.Transport != "" || e.Status >= 500 || e.Status == 429
}

// IsNotFound reports the one status a metadata source's "I have no such record"
// usually arrives as, so a provider can turn it into OutcomeNoMatch rather than
// into a failure.
func (e *FetchError) IsNotFound() bool { return e != nil && e.Status == 404 }

// Do performs one request and returns the response only when the source actually
// answered 2xx. Everything else is a [*FetchError].
func Do(ctx context.Context, h Host, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	resp, err := h.Fetch(ctx, req)
	if err != nil {
		return resp, &FetchError{URL: req.URL, Transport: err.Error()}
	}
	switch {
	case resp.Refused != "":
		return resp, &FetchError{URL: req.URL, Status: resp.Status, Refused: resp.Refused}
	case resp.Error != "":
		return resp, &FetchError{URL: req.URL, Status: resp.Status, Transport: resp.Error}
	case resp.Status < 200 || resp.Status > 299:
		return resp, &FetchError{URL: req.URL, Status: resp.Status}
	}
	return resp, nil
}

// DoJSON performs one request and decodes a 2xx body into out.
func DoJSON(ctx context.Context, h Host, req pluginapi.FetchRequest, out any) error {
	resp, err := Do(ctx, h, req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return &FetchError{URL: req.URL, Status: resp.Status, Decode: err}
	}
	return nil
}

// GetJSON is DoJSON for the call nine tenths of a metadata provider makes: a GET
// with a query and some headers, answering JSON.
func GetJSON(ctx context.Context, h Host, rawURL string, q url.Values, out any, headers ...pluginapi.FetchHeader) error {
	return DoJSON(ctx, h, pluginapi.FetchRequest{
		URL:     URLWithQuery(rawURL, q),
		Headers: headers,
	}, out)
}

// URLWithQuery joins a base URL and a query the way every provider in this
// repository already does, including the part each one got right separately: a
// base that already carries a query keeps it.
//
// It matters here more than it would in a server, because a plugin's base URL is
// the OPERATOR's — a mirror, a proxy, or in the test suite a URL carrying a mode
// marker — and dropping what they typed is how a plugin stops honouring an
// override.
func URLWithQuery(rawURL string, q url.Values) string {
	enc := q.Encode()
	if enc == "" {
		return rawURL
	}
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + enc
	}
	return rawURL + "?" + enc
}

// Header is a one-line constructor for the header list, so a call site reads as a
// list of headers rather than as a list of struct literals.
func Header(name, value string) pluginapi.FetchHeader {
	return pluginapi.FetchHeader{Name: name, Value: value}
}
