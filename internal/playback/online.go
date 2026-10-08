package playback

import (
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The negotiation for an Online item (ADR-0068 decision 4), reduced to the one
// choice this slice makes: which of a Plugin's muxed variants, if any, the client
// can play AS IS, so the Server can relay the bytes untouched.
//
// An Online item has no Edition, no File and no probed Streams — the Plugin states
// what a variant is — so none of the Title negotiation above is reachable from
// here, and nothing here is persisted. A variant the client cannot play, or one
// over the Playback ceiling, is not an error of the Plugin's: it is the
// "a transcode would be required" outcome the Title path reports, rendered by the
// api layer the same way. The ffmpeg path that satisfies it is a later slice.

// ChooseOnlineVariant returns the best muxed variant the client can play as-is
// within the constraints (the caller has already clamped them to the User's
// ceiling): the tallest one. With none playable it returns the Unsupported reason
// of the first variant the Plugin offered, or ReasonNoFile when it offered none
// this build can relay.
//
// A variant that states no height cannot be held to a resolution cap, so under a
// cap it is not playable: the ceiling must not be one a Plugin can step around by
// saying nothing.
func ChooseOnlineVariant(profile DeviceProfile, constraints Constraints, variants []pluginapi.OnlineVariant) (pluginapi.OnlineVariant, *Unsupported) {
	var best pluginapi.OnlineVariant
	bestHeight := -1
	var firstRefusal *Unsupported
	for _, v := range variants {
		if v.Kind != "" && v.Kind != pluginapi.OnlineVariantMuxed {
			continue
		}
		if v.URL == "" {
			continue
		}
		if why := onlineVariantRefusal(profile, constraints, v); why != nil {
			if firstRefusal == nil {
				firstRefusal = why
			}
			continue
		}
		if h := resolutionHeight(v.Resolution); h > bestHeight {
			best, bestHeight = v, h
		}
	}
	if bestHeight >= 0 {
		return best, nil
	}
	if firstRefusal != nil {
		return pluginapi.OnlineVariant{}, firstRefusal
	}
	return pluginapi.OnlineVariant{}, &Unsupported{Reason: ReasonNoFile, Detail: "no playable variant"}
}

// onlineVariantRefusal reports why the client cannot play v as-is, or nil.
func onlineVariantRefusal(profile DeviceProfile, c Constraints, v pluginapi.OnlineVariant) *Unsupported {
	if !profile.supportsContainer(v.Container) {
		return &Unsupported{Reason: ReasonContainer, Detail: "container " + NormalizeContainer(v.Container) + " not in device profile"}
	}
	height := resolutionHeight(v.Resolution)
	var video *VideoCodecSupport
	for _, codec := range v.Codecs {
		if s, ok := profile.videoCodec(codec); ok {
			s := s
			video = &s
			continue
		}
		if !profile.supportsAudio(codec) {
			reason := ReasonVideoCodec
			if looksLikeAudio(codec) {
				reason = ReasonAudioCodec
			}
			return &Unsupported{Reason: reason, Detail: "codec " + codec + " not in device profile"}
		}
	}
	if video == nil {
		return &Unsupported{Reason: ReasonNoVideo, Detail: "the variant names no video codec the device decodes"}
	}
	if cap := resolutionHeight(video.MaxResolution); cap > 0 && height > cap {
		return &Unsupported{Reason: ReasonResolution, Detail: "resolution " + v.Resolution + " exceeds the device's limit for " + video.Codec}
	}
	if cap := resolutionHeight(c.MaxResolution); cap > 0 && (height == 0 || height > cap) {
		return &Unsupported{Reason: ReasonResolution, Detail: "resolution " + v.Resolution + " exceeds the cap " + c.MaxResolution}
	}
	// A variant states no bitrate, so untouched bytes cannot be shown to be under a
	// bitrate cap. Under the Admin's ceiling the item is encoded held to it, or refused
	// without ffmpeg: the ceiling must not be one a Plugin can step around by saying
	// nothing. A bitrate only the client asked for is ignored (the web player always
	// sends one).
	if c.CeilingBitrate > 0 {
		return &Unsupported{Reason: ReasonBitrate, Detail: "the variant's bitrate is unknown and the bitrate ceiling is set"}
	}
	return nil
}

// looksLikeAudio says whether codec is one a refusal should be reported as an
// audio codec for. The profile names video and audio codecs apart, but a codec it
// lacks is in neither list, so the name is all there is to go on.
func looksLikeAudio(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "mp3", "opus", "vorbis", "flac", "ac3", "eac3", "dts", "alac", "pcm":
		return true
	}
	return false
}

// ResolutionHeight maps a resolution token ("720p") to its pixel height, 0 when
// the token is empty or unknown.
func ResolutionHeight(token string) int { return resolutionHeight(token) }

// OnlinePlan is how the Server will play an Online item: relay the bytes of a muxed
// variant the client can play as is, or have ffmpeg read the variant and encode it
// for the client.
type OnlinePlan struct {
	Variant pluginapi.OnlineVariant
	// Transcode is true for ffmpeg, false for the relay.
	Transcode bool
	// MaxHeight and MaxBitrate bound the encode: the constraints the caller already
	// clamped to the User's Playback ceiling. 0 is no bound.
	MaxHeight  int
	MaxBitrate int64
}

// PlanOnline chooses between the relay (B) and ffmpeg (C). A muxed variant the
// client can play untouched is relayed. Otherwise, when ffmpeg is there to be had,
// the best variant of ANY shape goes to it: the tallest within the ceiling, else
// the smallest above it, scaled down to the ceiling by the encode. With no ffmpeg
// the answer is the refusal ChooseOnlineVariant gives, which the api layer renders
// as a Title's "a transcode would be required".
//
// Governance (the transcode cap, ADR-0009) is not decided here: it is a question of
// how many are running, which the caller asks of the session Manager.
func PlanOnline(profile DeviceProfile, constraints Constraints, variants []pluginapi.OnlineVariant, canTranscode bool) (OnlinePlan, *Unsupported) {
	relay, refusal := ChooseOnlineVariant(profile, constraints, variants)
	if refusal == nil {
		return OnlinePlan{Variant: relay}, nil
	}
	if !canTranscode {
		return OnlinePlan{}, refusal
	}
	capHeight := reencodeCapHeight(profile, constraints)
	var best pluginapi.OnlineVariant
	found := false
	for _, v := range variants {
		if !hasOnlineSource(v) {
			continue
		}
		if !found || betterForTranscode(v, best, capHeight) {
			best, found = v, true
		}
	}
	if !found {
		return OnlinePlan{}, refusal
	}
	return OnlinePlan{Variant: best, Transcode: true, MaxHeight: capHeight, MaxBitrate: constraints.MaxBitrate}, nil
}

// OnlineServerBusy is the rejection an Online ffmpeg play gets at a full transcode
// cap, the same ServerBusy a Title gets. There is no estimate to halve for a remote
// variant, so the suggestion starts from the bitrate the client asked to be held to
// (the floor when it asked for none).
func OnlineServerBusy(c Constraints) *ServerBusy {
	return &ServerBusy{SuggestedMaxBitrate: suggestBusyBitrate(0, c.MaxBitrate)}
}

// hasOnlineSource says v names what its shape needs to be read.
func hasOnlineSource(v pluginapi.OnlineVariant) bool {
	switch v.Kind {
	case "", pluginapi.OnlineVariantMuxed, pluginapi.OnlineVariantManifest:
		return v.URL != ""
	case pluginapi.OnlineVariantSplit:
		return v.VideoURL != "" && v.AudioURL != ""
	}
	return false
}

// betterForTranscode reports whether v beats cur as the source of an encode under
// capHeight (0 = none): any variant within the cap beats one above it, the tallest
// within wins, and above the cap the smallest wins since it is the least to decode.
// An unstated height counts as the smallest within the cap. The first of equals is
// kept.
func betterForTranscode(v, cur pluginapi.OnlineVariant, capHeight int) bool {
	hv, hc := resolutionHeight(v.Resolution), resolutionHeight(cur.Resolution)
	if capHeight <= 0 {
		return hv > hc
	}
	vIn, cIn := hv <= capHeight, hc <= capHeight
	switch {
	case vIn && !cIn:
		return true
	case !vIn && cIn:
		return false
	case vIn:
		return hv > hc
	default:
		return hv < hc
	}
}
