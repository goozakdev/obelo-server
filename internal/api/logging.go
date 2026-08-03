package api

import (
	"io"
	"log"
	"net/http"
	"time"
)

// The access log.
//
// It is deliberately small and it is deliberately here, in the package that
// knows which parts of a URL are secrets, rather than in the composition root
// that does not. Two of this server's URLs carry a credential:
//
//   - GET /files/{id}/download?token=…    the ACCOUNT session token, in the query
//     (requireAuthAllowQueryToken — scoped to that one route, and staying that way)
//   - GET /stream/{streamToken}/…         the session stream token, in the path
//     (.scratch/session-stream-tokens)
//
// A request logger that prints r.URL is the single most likely way either of
// those reaches a file on disk, and printing r.URL is what every request logger
// does. So this one prints neither raw: the query string is dropped ENTIRELY
// (nothing in it is worth a log line and one value in it is an account
// credential), and the path goes through RedactPath.

// LogRequests wraps h with the server's access log: one line per request with
// the method, status, response size, duration, and a REDACTED path. It is
// applied by the binary (cmd/juicebox) around the fully composed handler; the
// test harness serves the handler unwrapped, so the suite stays quiet.
//
// Wrapping is otherwise transparent: the ResponseWriter passthrough keeps
// http.Flusher (the SSE stream at GET /events type-asserts it) and
// io.ReaderFrom (http.ServeContent's copy path for the progressive stream)
// working, and exposes Unwrap for http.ResponseController.
func LogRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &accessLogWriter{ResponseWriter: w}
		h.ServeHTTP(lw, r)
		// RequestURI rather than URL.Path: it is the raw request line, so it is what
		// a naive logger would reach for, and routing it through RedactPath here is
		// the point of this function. Nothing prints the query string.
		log.Printf("juicebox: http: %s %s %d %dB %s",
			r.Method, RedactPath(pathOnly(r.RequestURI)), lw.statusCode(), lw.written, time.Since(start).Round(time.Millisecond))
	})
}

// pathOnly strips the query string from a request-target. The query is never
// logged: ?token= on the direct-file download is the account credential, and no
// other query parameter this server accepts is worth the risk of an exception
// that someone later widens.
func pathOnly(requestURI string) string {
	for i := 0; i < len(requestURI); i++ {
		if requestURI[i] == '?' {
			return requestURI[:i]
		}
	}
	return requestURI
}

// accessLogWriter records the status and byte count of a response without
// altering it.
type accessLogWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

// statusCode reports the status actually sent: a handler that writes a body
// without calling WriteHeader has implicitly sent 200, and one that wrote
// nothing at all still ends as 200 once the handler returns.
func (w *accessLogWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *accessLogWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Flush keeps the SSE broker's stream (GET /events) working: that handler type-
// asserts http.Flusher on the writer it is given, and a wrapper that dropped the
// assertion would silently turn realtime updates into a stalled connection.
func (w *accessLogWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom keeps http.ServeContent's copy path on the underlying writer (the
// progressive /stream route moves whole files through it) instead of forcing it
// through a buffered fallback, and counts the bytes on the way past.
func (w *accessLogWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.written += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.written += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController (read/write
// deadlines, explicit flushes) so a future handler is not blocked by this
// wrapper.
func (w *accessLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
