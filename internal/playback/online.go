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
