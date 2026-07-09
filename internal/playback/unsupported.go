package playback

// Reason is a machine-readable explanation of why a File cannot be direct-played
// — i.e. why a transcode would be required. It is carried in the
// TRANSCODE_REQUIRED error's details so a client can show a precise message and a
// later transcode tier knows what to convert.
type Reason string

const (
	// ReasonContainer: the device cannot demux the File's container.
	ReasonContainer Reason = "container"
	// ReasonVideoCodec: the device cannot decode the File's video codec.
	ReasonVideoCodec Reason = "videoCodec"
	// ReasonAudioCodec: the device cannot decode the selected audio Stream's codec.
	ReasonAudioCodec Reason = "audioCodec"
	// ReasonResolution: the video resolution exceeds a device or session ceiling.
	ReasonResolution Reason = "resolution"
	// ReasonBitrate: the File's bitrate exceeds the session maxBitrate.
	ReasonBitrate Reason = "bitrate"
	// ReasonAudioChannels: the audio channel count exceeds the device max.
	ReasonAudioChannels Reason = "audioChannels"
	// ReasonNoVideo: the File has no video Stream (not a playable Movie File here).
	ReasonNoVideo Reason = "noVideo"
	// ReasonNoFile: no present File to play (all Missing, or unknown editionId).
	ReasonNoFile Reason = "noFile"
)

// Unsupported is the negotiation failure: the client cannot direct-play the
// File, with the first blocking Reason and a human-readable detail. The api
// layer maps it to the structured TRANSCODE_REQUIRED error (501-class). It is a
// value error (not a Go error) because it is an expected negotiation outcome, not
// a fault — there is no remux/transcode tier in this slice to fall back to.
type Unsupported struct {
	Reason Reason
	Detail string
}

// Error implements error so callers may treat it as one if convenient.
func (u *Unsupported) Error() string {
	if u == nil {
		return ""
	}
	return "transcode required: " + string(u.Reason) + ": " + u.Detail
}
