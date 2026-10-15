// Command fetchpayload is the build-time half of the envelope-embed path
// (.claude/scratch/build-fetch-bootstrap-keys): it fetches the sealed rotation
// envelope from OBELO_BOOTSTRAP_ENVELOPE_URL, validates it, and prints ONLY the
// envelope's base64 ciphertext `payload` to stdout, for `make`/the Dockerfile to
// inject into config.bootstrapEnvelope with -ldflags -X.
//
// Validation is fail-closed: a non-200, a version other than rotation.SupportedVersion,
// a missing OBELO_APP_ENC_KEY, or a payload that OBELO_APP_ENC_KEY cannot decrypt all
// exit 1 with a message on stderr, so a build never ships an envelope the binary could
// not read. The decrypted keys are checked, never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/goozakdev/obelo-server/internal/rotation"
)

const maxEnvelopeBytes = 64 << 10

func main() {
	url := os.Getenv("OBELO_BOOTSTRAP_ENVELOPE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "ERROR: OBELO_BOOTSTRAP_ENVELOPE_URL is empty")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	payload, err := fetchPayload(ctx, url, os.Getenv("OBELO_APP_ENC_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: fetching the bootstrap envelope: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(payload)
}

// fetchPayload returns the envelope's ciphertext payload verbatim after checking it
// is servable (HTTP 200, supported version) and decryptable under encKeyB64.
func fetchPayload(ctx context.Context, url, encKeyB64 string) (string, error) {
	if encKeyB64 == "" {
		return "", errors.New("OBELO_APP_ENC_KEY is empty; it is required to verify the envelope (and to decrypt it at runtime)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// The Worker 404s any User-Agent not starting with "obelo/".
	req.Header.Set("User-Agent", "obelo/build")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered HTTP %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelopeBytes))
	if err != nil {
		return "", err
	}
	var env rotation.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("envelope is not valid JSON: %w", err)
	}
	if env.V != rotation.SupportedVersion {
		return "", fmt.Errorf("envelope version %d unsupported, want %d", env.V, rotation.SupportedVersion)
	}
	if _, err := rotation.Decrypt(encKeyB64, env.Payload); err != nil {
		return "", fmt.Errorf("cannot decrypt payload with OBELO_APP_ENC_KEY: %w", err)
	}
	return env.Payload, nil
}
