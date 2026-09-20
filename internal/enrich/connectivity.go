package enrich

import (
	"context"
	"errors"
	"net/url"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestConnection performs a best-effort, single-shot connectivity/credential
// probe for one provider using the supplied (current-or-edited) credentials — the
// one place the settings surface makes a real outbound call, and only on an
// explicit Admin action (metadata-providers 02). It constructs just that
// provider (never the whole chain) and issues one representative Lookup: a normal
// result OR ErrNoMatch means the host answered and any key was accepted (ok); a
// transport/credential error means it did not (not ok, with the error as detail).
// The caller supplies a bounded context so a hung host can't stall the request.
// A key-requiring provider with no key fails fast without any call.
//
// WHAT TO LOOK UP IS THE PLUGIN'S OWN DECLARATION (ADR-0059 decision 8), carried
// on Descriptor.Probe. This used to be a switch over eight slugs, each with a
// reference the author of THIS file happened to know that source could answer,
// and an Installed provider fell off the end of it into "unknown provider" —
// which is to say the host's most operator-visible provider feature only worked
// for the providers the host was written around. The judgment stays the host's
// and is unchanged: matched or no-match proves the host was reached and the
// credential accepted; unavailable, a refusal or a fetch error is a failed test
// with the reason as the sentence.
//
// Every source is probed BY BUILDING ITS PLUGIN from the catalog (ADR-0057): the
// probe exercises the registration and the adapter — the same path the real chain
// takes — rather than a private construction no production flow uses.
//
// A PROVIDER WITH TWO HOSTS GETS TWO CALLS (.scratch/bundled-plugins issue 06).
// imageBaseURL is the operator's current-or-edited SECOND host, and a Descriptor
// that declares one has the artwork-candidates call issued against the same probe
// reference after the lookup — see the rule below, which is where it is written
// down and why.
func TestConnection(ctx context.Context, cat Catalog, slug, apiKey, baseURL, imageBaseURL, language string) (ok bool, detail string) {
	entry, found := cat.Entry(slug)
	if !found {
		return false, "unknown provider"
	}
	if entry.RequiresKey && apiKey == "" {
		return false, "an API key is required to test this provider"
	}
	base := baseURL
	if base == "" {
		base = entry.DefaultURL
	}
	imageBase := imageBaseURL
	if imageBase == "" {
		imageBase = entry.DefaultURL2
	}

	settings := pluginapi.Settings{
		Enabled:  true,
		Secret:   apiKey,
		URL:      base,
		URL2:     imageBase,
		Language: language,
	}

	// THERE ARE NO SPECIAL CASES LEFT (.scratch/bundled-plugins: issue 06). The last
	// one was Cover Art Archive's, which had no Plugin of its own and was probed by
	// building MusicBrainz with the supplied host as its second URL; that host is now
	// the MusicBrainz plugin's own, so the two rows became one and this function
	// treats every provider alike.
	if entry.Probe == nil {
		return false, "this provider declares no connection probe"
	}
	provider := cat.buildPlugin(slug, settings)
	if provider == nil {
		return false, "this provider cannot be built from these settings"
	}

	probe := titleRefFromWire(*entry.Probe)
	record, err := provider.Lookup(ctx, probe)
	if err != nil && !errors.Is(err, ErrNoMatch) {
		return false, err.Error()
	}

	// A PROVIDER WITH TWO HOSTS IS TESTED AGAINST BOTH (ADR-0059 decision 8, as issue
	// 06 extends it). A lookup proves the API host answered and any credential was
	// accepted, and for a single-host source that is the whole of what a connection
	// test can prove. It proves NOTHING about a second host: a source whose images
	// come from an image CDN reaches that CDN only when something asks for images,
	// and the probe's lookup does not — MusicBrainz's cover URL is BUILT from URL2
	// and never fetched, so a wrong Cover Art Archive host passed the test and then
	// failed silently on every album cover the pass downloaded.
	//
	// So a Descriptor that declares a second URL gets a SECOND CALL, and the rule is
	// the call the provider already has for it: artwork-candidates, for the record
	// the probe just resolved. That is the one contract call whose whole subject is
	// images, so the host it reaches IS the second host by construction rather than
	// by this function guessing a URL shape. A provider that declares no URL2, or no
	// artwork-candidates capability, is untouched and costs exactly one call as
	// before — which is every provider this server ships but two.
	//
	// It is the RESOLVED id that makes it work: an image set is keyed by the record,
	// so the probe reference alone lists nothing and would reach no host at all. That
	// is also why a Plugin's probe should name something whose images exist —
	// MusicBrainz's is an album, the reference the Cover Art Archive's own
	// registration used to declare before this host became MusicBrainz's second URL.
	//
	// The refusal names the host under test, because "connection failed" on a screen
	// with two URL fields does not tell an operator which one they typed wrong.
	// ErrSearchUnavailable is NOT forgiven below, which is the one place this call is
	// judged differently from the lookup. It is what a Plugin's OutcomeUnavailable
	// becomes, and for a capability the Descriptor DECLARES — which is what the check
	// below tests first — "I cannot serve this call" IS the image host failing to
	// answer, which is the whole thing under test. A no-match is a pass, as it is for
	// the lookup: a record with no images is an answer, and the host that gave it was
	// reached.
	if entry.DefaultURL2 == "" || !entry.HasCapability(pluginapi.CapabilityArtworkCandidates) {
		return true, "connection succeeded"
	}
	if _, err := provider.ArtworkCandidates(ctx, probeRefWithID(probe, slug, record), "cover"); err != nil &&
		!errors.Is(err, ErrNoMatch) {
		return false, imageHostFailure(imageBase, err)
	}
	return true, "connection succeeded"
}

// probeRefWithID puts the id the probe lookup resolved into the reference the image
// call is made with, under the External-id namespace it belongs to: the record's
// Source, which names its ExternalID's namespace, or — for a record that names
// none — the Plugin's own id, which is a source's namespace (ADR-0060 decision 1).
//
// It sets the MAP, and wireRefFromTitleRef fills the named v1 mirror from it for the
// five shipped namespaces, so a v1 guest reading MusicbrainzID and a new one reading
// ref.ID("musicbrainz") see the same id. This used to fill EVERY named field with
// the one id, because the contract had no way to say which namespace it was in; now
// it does, and a Plugin asked about its own record finds it where it reads.
//
// An empty id leaves the reference alone: a probe that resolved nothing (a no-match,
// which is a PASS for the credential test) has no record to ask for images of, and
// the call then costs whatever a Plugin charges for a reference it cannot use, which
// for every shipped one is nothing.
func probeRefWithID(ref TitleRef, pluginID string, record TitleMetadata) TitleRef {
	id := strings.TrimSpace(record.ExternalID)
	if id == "" {
		return ref
	}
	ns := strings.TrimSpace(record.Source)
	if ns == "" {
		ns = pluginID
	}
	ids := make(map[string]string, len(ref.ExternalIDs)+1)
	for k, v := range ref.ExternalIDs {
		ids[k] = v
	}
	ids[ns] = id
	ref.ExternalIDs = ids
	return ref
}

// imageHostFailure is the sentence a failed second-host probe reports. It names the
// host the operator is being asked about, because a provider dialog with two URL
// fields and one verdict is a dialog that cannot be acted on.
func imageHostFailure(imageBase string, err error) string {
	host := strings.TrimSpace(imageBase)
	if u, perr := url.Parse(host); perr == nil && u.Host != "" {
		host = u.Host
	}
	if host == "" {
		return "the image host could not be reached: " + err.Error()
	}
	return "the image host " + host + " could not be reached: " + err.Error()
}
