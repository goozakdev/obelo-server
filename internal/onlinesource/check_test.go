package onlinesource

import (
	"context"
	"errors"
	"net"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// checkedService is a Service whose first-URL policy allows the hosts in allow and
// resolves every name to answer.
func checkedService(allow map[string]bool, answer ...string) *Service {
	s := New(pluginapi.NewRegistry(), nil)
	s.SetMediaHostPolicy(func(_, host string) bool { return allow[host] })
	s.lookup = func(_ context.Context, _ string) ([]net.IP, error) {
		var ips []net.IP
		for _, a := range answer {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
	return s
}

// TestAnHTTPFirstURLIsRefused: a build that trusted the Plugin's scheme would let
// a plain-http media URL through to ffmpeg.
func TestAnHTTPFirstURLIsRefused(t *testing.T) {
	s := checkedService(map[string]bool{"cdn.example.test": true}, "93.184.216.34")
	for _, u := range []string{
		"http://cdn.example.test/v.mp4", "ftp://cdn.example.test/v.mp4", "file:///etc/passwd",
		"//cdn.example.test/v.mp4", "cdn.example.test/v.mp4", "https:///v.mp4", "https://u:p@cdn.example.test/v.mp4", "",
	} {
		if err := s.checkMediaURL(context.Background(), "tube", u); !errors.Is(err, ErrMediaRefused) {
			t.Errorf("checkMediaURL(%q) = %v, want ErrMediaRefused", u, err)
		}
	}
	if err := s.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/v.mp4"); err != nil {
		t.Fatalf("an https URL on an allowlisted host was refused: %v", err)
	}
}

// TestAHostOffTheAllowlistIsRefused: the manifest allowlist decides, for the source
// that asked; with no policy wired nothing is allowed.
func TestAHostOffTheAllowlistIsRefused(t *testing.T) {
	s := checkedService(map[string]bool{"cdn.example.test": true}, "93.184.216.34")
	if err := s.checkMediaURL(context.Background(), "tube", "https://elsewhere.example.test/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Fatalf("a host off the allowlist = %v, want ErrMediaRefused", err)
	}
	asked := ""
	s.SetMediaHostPolicy(func(source, host string) bool { asked = source + "/" + host; return true })
	if err := s.checkMediaURL(context.Background(), "tube", "https://CDN.Example.Test:8443/v.mp4"); err != nil {
		t.Fatalf("allowlisted host with a port was refused: %v", err)
	}
	if asked != "tube/cdn.example.test" {
		t.Fatalf("policy asked about %q, want the source and the lower-case host without its port", asked)
	}

	bare := New(pluginapi.NewRegistry(), nil)
	if err := bare.checkMediaURL(context.Background(), "tube", "https://93.184.216.34/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Fatalf("with no allowlist wired = %v, want ErrMediaRefused (fail closed)", err)
	}
}

// TestAnAllowlistedHostResolvingToAPrivateAddressIsRefused: loopback, private,
// link-local (cloud metadata), CGNAT and the IPv6 equivalents, and a mixed answer
// with one public address among them.
func TestAnAllowlistedHostResolvingToAPrivateAddressIsRefused(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.9", "172.16.5.5", "169.254.169.254", "100.64.1.1", "0.0.0.0", "::1", "fd00::1", "fe80::1"} {
		s := checkedService(map[string]bool{"cdn.example.test": true}, bad)
		if err := s.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/v.mp4"); !errors.Is(err, ErrMediaRefused) {
			t.Errorf("host resolving to %s = %v, want ErrMediaRefused", bad, err)
		}
	}
	mixed := checkedService(map[string]bool{"cdn.example.test": true}, "93.184.216.34", "10.0.0.1")
	if err := mixed.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Errorf("a name with one private answer = %v, want ErrMediaRefused", err)
	}
	none := checkedService(map[string]bool{"cdn.example.test": true})
	if err := none.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Errorf("a name with no address = %v, want ErrMediaRefused", err)
	}
	failing := checkedService(map[string]bool{"cdn.example.test": true})
	failing.lookup = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no such host") }
	if err := failing.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Errorf("a name that will not resolve = %v, want ErrMediaRefused (fail closed)", err)
	}
	literal := checkedService(map[string]bool{"127.0.0.1": true}, "127.0.0.1")
	if err := literal.checkMediaURL(context.Background(), "tube", "https://127.0.0.1/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Errorf("an allowlisted loopback literal = %v, want ErrMediaRefused", err)
	}
}

// TestOnlyTheFirstURLIsChecked: the ffmpeg path judges the URL the Plugin gave and
// nothing after it. The check has no notion of a later hop: a first URL that passes
// is accepted whatever it redirects to, which is the accepted residual risk
// (ADR-0068 decision 9). The lookup is called once, for the first host.
func TestOnlyTheFirstURLIsChecked(t *testing.T) {
	lookups := 0
	s := checkedService(map[string]bool{"cdn.example.test": true}, "93.184.216.34")
	inner := s.lookup
	s.lookup = func(ctx context.Context, h string) ([]net.IP, error) { lookups++; return inner(ctx, h) }
	if err := s.checkMediaURL(context.Background(), "tube", "https://cdn.example.test/redirects-to-the-lan.m3u8"); err != nil {
		t.Fatalf("first URL refused: %v", err)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want exactly 1 (the first host; no later hop is re-validated)", lookups)
	}
}

// TestAnExemptedAddressSkipsOnlyTheAddressCheck: tests serve media from 127.0.0.1;
// the exemption removes the address check and keeps https and the allowlist.
func TestAnExemptedAddressSkipsOnlyTheAddressCheck(t *testing.T) {
	s := checkedService(map[string]bool{"127.0.0.1": true}, "127.0.0.1")
	s.ExemptMediaAddrs("127.0.0.1:8443")
	if err := s.checkMediaURL(context.Background(), "tube", "https://127.0.0.1:8443/v.mp4"); err != nil {
		t.Fatalf("exempted address refused: %v", err)
	}
	if err := s.checkMediaURL(context.Background(), "tube", "https://127.0.0.1:9/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Fatalf("another port on the same host = %v, want ErrMediaRefused", err)
	}
	if err := s.checkMediaURL(context.Background(), "tube", "http://127.0.0.1:8443/v.mp4"); !errors.Is(err, ErrMediaRefused) {
		t.Fatalf("exempted address over http = %v, want ErrMediaRefused", err)
	}
}
