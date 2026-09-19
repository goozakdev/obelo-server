package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// --- the server process -----------------------------------------------------

// server is one Obelo binary running against one data directory on a loopback
// port of its own. Never the developer's server: the port is chosen at runtime
// and the data directory is always under this run's temp root.
type server struct {
	bin     string
	dataDir string
	logPath string
	base    string
	cmd     *exec.Cmd
}

// freePort asks the kernel for a port and immediately gives it back. There is a
// race between here and the server's own bind, and it is the right trade: the
// alternative is a fixed port, and a fixed port is how a test harness stomps on
// something the developer is running.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// startServer boots bin against dataDir with env on top of a deliberately small
// environment. extraEnv wins over the base env.
func startServer(bin, dataDir, logPath string, extraEnv map[string]string) (*server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	env := map[string]string{
		"OBELO_DATA_DIR":    dataDir,
		"OBELO_LISTEN_ADDR": fmt.Sprintf("127.0.0.1:%d", port),
		// No outbound calls this run did not ask for: the ADR-0032 rotation
		// client would otherwise reach for the real internet on boot.
		"OBELO_KEY_ROTATION": "off",
		// The consent gate (ADR-0032) is granted up front so both builds are
		// enriching from their first boot and the gate is not a variable.
		"OBELO_ENRICHMENT_CONSENT": "granted",
		// No scheduled work. A background sweep firing mid-run would add
		// provider requests to a counter this whole exercise is reading.
		"OBELO_SCAN_INTERVAL":   "0",
		"OBELO_ENRICH_INTERVAL": "0",
		"OBELO_AUTO_ENRICH":     "true",
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), envPairs(env)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{
		bin:     bin,
		dataDir: dataDir,
		logPath: logPath,
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
	}
	if err := s.waitReady(90 * time.Second); err != nil {
		s.stop()
		return nil, fmt.Errorf("%s did not come up: %w (see %s)", bin, err, logPath)
	}
	return s, nil
}

func envPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func (s *server) waitReady(limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.base + "/api/v1/server")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("process exited: %v", s.cmd.ProcessState)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return last
}

// stop shuts the server down the way an operator would, and waits for it,
// because the next phase opens the same SQLite file.
func (s *server) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

var claimTokenRE = regexp.MustCompile(`first-Admin claim token: (\S+)`)

// claimToken reads the one-time first-Admin token out of the server's own log,
// which is where ADR-0013 puts it.
func (s *server) claimToken() (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(s.logPath)
		if err == nil {
			if m := claimTokenRE.FindSubmatch(b); m != nil {
				return string(m[1]), nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("no claim token in %s", s.logPath)
}

// --- the API client ---------------------------------------------------------

type client struct {
	base  string
	token string
	hc    *http.Client
}

func newClient(base string) *client {
	return &client{base: base, hc: &http.Client{Timeout: 120 * time.Second}}
}

// do issues one API call. body may be nil; out may be nil. A non-2xx is an
// error carrying the response body, because every refusal in this server says
// something useful and swallowing it turns a five-second fix into an hour.
func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+"/api/v1"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w (%s)", method, path, err, truncate(string(raw), 400))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// setupAndLogin performs first-run setup and logs in, returning with the
// client's bearer token set.
func (c *client) setupAndLogin(claim string) error {
	if err := c.do(http.MethodPost, "/setup", map[string]any{
		"claimToken": claim,
		"username":   "differential",
		"password":   "differential-pass-0918",
	}, nil); err != nil {
		return err
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := c.do(http.MethodPost, "/auth/login", map[string]any{
		"username": "differential",
		"password": "differential-pass-0918",
		"device": map[string]any{
			"clientId": "differential-driver",
			"name":     "differential",
			"platform": "cli",
		},
	}, &login); err != nil {
		return err
	}
	if login.Token == "" {
		return fmt.Errorf("login returned no token")
	}
	c.token = login.Token
	return nil
}

func (c *client) login() error {
	var login struct {
		Token string `json:"token"`
	}
	if err := c.do(http.MethodPost, "/auth/login", map[string]any{
		"username": "differential",
		"password": "differential-pass-0918",
		"device": map[string]any{
			"clientId": "differential-driver",
			"name":     "differential",
			"platform": "cli",
		},
	}, &login); err != nil {
		return err
	}
	c.token = login.Token
	return nil
}

// --- scan / enrich, and waiting for them ------------------------------------

type scanStatus struct {
	State       string `json:"state"`
	TitlesFound int    `json:"titlesFound"`
	FilesFound  int    `json:"filesFound"`
	FinishedAt  string `json:"finishedAt"`
	Error       string `json:"errorMessage"`
}

type enrichResult struct {
	Total      int    `json:"total"`
	Matched    int    `json:"matched"`
	Unmatched  int    `json:"unmatched"`
	Failed     int    `json:"failed"`
	Disabled   int    `json:"disabled"`
	Retrying   int    `json:"retrying"`
	Mode       string `json:"mode"`
	FinishedAt string `json:"finishedAt"`
}

type enrichPass struct {
	State    string        `json:"state"`
	Started  bool          `json:"started"`
	LastPass *enrichResult `json:"lastPass"`
}

// scanAndSettle runs a scan, waits for it, waits for the automatic enrichment
// pass it triggers, then runs one explicit pass in the given mode and waits for
// that too. mode "" is the server's default ("new").
//
// It returns the pass that DID the work — the automatic one, when there was
// anything to do — because that is the number a reader wants to see.
func (c *client) scanAndSettle(libID, mode string) (*enrichResult, error) {
	if err := c.do(http.MethodPost, "/libraries/"+libID+"/scan", nil, nil); err != nil {
		return nil, err
	}
	if err := c.waitScan(libID); err != nil {
		return nil, err
	}
	// The post-scan automatic pass (autoEnrichAfterScan) is already in flight;
	// let it finish before asking for one of our own, so the two do not
	// interleave and the counters belong to a settled state.
	auto, err := c.waitEnrich(libID)
	if err != nil {
		return nil, err
	}
	explicit, err := c.enrichAndSettle(libID, mode)
	if err != nil {
		return nil, err
	}
	if auto != nil && auto.Total > 0 {
		return auto, nil
	}
	return explicit, nil
}

// enrichAndSettle asks for one pass and waits until the server is quiet again.
func (c *client) enrichAndSettle(libID, mode string) (*enrichResult, error) {
	path := "/libraries/" + libID + "/enrich"
	if mode != "" {
		path += "?mode=" + mode
	}
	if err := c.do(http.MethodPost, path, nil, nil); err != nil {
		return nil, err
	}
	// Give the worker a moment to pick the pass up, so the quiet rule below
	// cannot mistake "not started yet" for "finished".
	time.Sleep(400 * time.Millisecond)
	return c.waitEnrich(libID)
}

func (c *client) waitScan(libID string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var st scanStatus
		if err := c.do(http.MethodGet, "/libraries/"+libID+"/scan", nil, &st); err != nil {
			return err
		}
		if st.State != "running" && st.FinishedAt != "" {
			if st.Error != "" {
				return fmt.Errorf("scan of %s failed: %s", libID, st.Error)
			}
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("scan of %s did not finish in 10m", libID)
}

// quietPolls is how many consecutive idle reads, at pollInterval apart, count as
// "the pass has settled".
//
// The obvious rule — wait for lastPass.finishedAt to CHANGE — does not work, and
// finding that out cost a hang: finishedAt is second-granular, so a pass that
// starts and finishes inside the same second as the previous one is
// indistinguishable from no pass at all. A quiet period is the honest signal,
// and it also covers the case this harness cares most about, which is a pass
// that legitimately has nothing to do and therefore records nothing new.
const (
	quietPolls   = 5
	pollInterval = 300 * time.Millisecond
)

// waitEnrich waits until the Library has been idle for quietPolls consecutive
// reads and returns the last pass it recorded (nil if it has never run one).
func (c *client) waitEnrich(libID string) (*enrichResult, error) {
	deadline := time.Now().Add(15 * time.Minute)
	quiet := 0
	var last *enrichResult
	for time.Now().Before(deadline) {
		var p enrichPass
		if err := c.do(http.MethodGet, "/libraries/"+libID+"/enrich", nil, &p); err != nil {
			return nil, err
		}
		if p.State == "idle" {
			quiet++
			last = p.LastPass
			if quiet >= quietPolls {
				return last, nil
			}
		} else {
			quiet = 0
		}
		time.Sleep(pollInterval)
	}
	return nil, fmt.Errorf("enrichment of %s did not settle in 15m", libID)
}
