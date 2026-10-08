package onlinesource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/goozakdev/obelo-server/internal/safefetch"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The host's judgment on the FIRST URL of a variant (ADR-0068 decision 9), made
// before any session exists and for every URL the Plugin supplied for the media
// (a muxed file, a split variant's video and audio, a manifest): https, a host the
// Plugin's manifest allowlist licenses (exact, or under a domain-suffix entry), and
// no address that host resolves to is loopback, private, link-local or otherwise
// not a public one.
//
// This is the whole of the ffmpeg path's check. ffmpeg fetches the URL itself, and
// neither a redirect off it nor an HLS/DASH segment hop is looked at again. That is
// accepted and documented (docs/plugins/authoring.md, ADR-0068), as is the second
// residual risk: the lookup here is the Server's own and ffmpeg resolves the name
// again, so a rebinding answer can pass this and still reach the LAN. The relay
// path does not share either risk; it fetches through safefetch, hop by hop.

// ErrMediaRefused: a variant's URL failed the host's judgment.
var ErrMediaRefused = errors.New("onlinesource: media URL refused")

// SetMediaHostPolicy installs the allowlist question: whether sourceID's manifest
// licenses host as a media host. With none installed no media URL is allowed.
func (s *Service) SetMediaHostPolicy(allow func(sourceID, host string) bool) { s.mediaAllow = allow }

// ExemptMediaAddrs lists host:port addresses whose ADDRESS check is skipped (TESTS
// ONLY: the suite serves media from a loopback httptest server). https and the
// allowlist still apply to them.
func (s *Service) ExemptMediaAddrs(addrs ...string) {
	if s.exemptAddrs == nil {
		s.exemptAddrs = map[string]bool{}
	}
	for _, a := range addrs {
		s.exemptAddrs[a] = true
	}
}

// checkMediaURL returns nil when raw may be handed to ffmpeg or the relay, and an
// ErrMediaRefused naming why otherwise. The reason is for the log; a client is
// told only that the source is not responding.
func (s *Service) checkMediaURL(ctx context.Context, sourceID, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("%w: not an https URL", ErrMediaRefused)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if s.mediaAllow == nil || !s.mediaAllow(sourceID, host) {
		return fmt.Errorf("%w: host %s is not on the source's allowlist", ErrMediaRefused, host)
	}
	if s.exemptAddrs[u.Host] {
		return nil
	}
	ips, err := s.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("%w: host %s does not resolve", ErrMediaRefused, host)
	}
	for _, ip := range ips {
		if safefetch.IsBlockedAddress(ip) {
			return fmt.Errorf("%w: host %s resolves to a non-public address", ErrMediaRefused, host)
		}
	}
	return nil
}

// checkVariant judges every URL a variant names for the media.
func (s *Service) checkVariant(ctx context.Context, sourceID string, v pluginapi.OnlineVariant) error {
	var urls []string
	switch v.Kind {
	case "", pluginapi.OnlineVariantMuxed, pluginapi.OnlineVariantManifest:
		urls = []string{v.URL}
	case pluginapi.OnlineVariantSplit:
		urls = []string{v.VideoURL, v.AudioURL}
	default:
		return fmt.Errorf("%w: unknown variant kind %q", ErrMediaRefused, v.Kind)
	}
	for _, u := range urls {
		if err := s.checkMediaURL(ctx, sourceID, u); err != nil {
			return err
		}
	}
	return nil
}

func lookupIPs(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}
