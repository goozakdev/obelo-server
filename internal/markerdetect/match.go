package markerdetect

// Params are the matching thresholds. DefaultParams is what the Server runs;
// they are a struct rather than constants so a test can say which it depends on.
type Params struct {
	// MaxBitErrors is how many of a frame's 32 bits may differ for two frames to
	// count as the same sound. Unrelated audio differs in about 16.
	MaxBitErrors int
	// MaxGapFrames is how many frames in a row may disagree inside a shared run
	// before it is considered over — a click, a dropped frame, a line of dialogue
	// laid over the theme.
	MaxGapFrames int
	// MinSpanMs is the shortest shared stretch that counts as an Intro or Credits.
	// Two episodes can share a second of a sting or a laugh; they do not share
	// fifteen seconds of anything by accident.
	MinSpanMs int64
	// MaxIntroMs is the longest shared stretch near the start still believed to be
	// an Intro. Two Files sharing more than this at the start are more likely the
	// same episode twice, and skipping minutes of it would be worse than nothing.
	MaxIntroMs int64
	// MinChangingFraction is how much of a shared stretch must be sound that keeps
	// changing: the share of its frames whose print differs from the frame before
	// by more than MaxBitErrors. A steady tone or hum prints nearly the same word
	// every frame, whatever its pitch, so two episodes that both hum would
	// otherwise "share" it; music and speech change most frames.
	MinChangingFraction float64
	// AgreeMs is how far apart two candidate spans of one File may start and end
	// and still be the same span, found by comparing it with different episodes.
	AgreeMs int64
	// WindowFraction and MaxWindowMs bound how much of each end of a File is
	// listened to: the first and last WindowFraction of it, at most MaxWindowMs.
	WindowFraction float64
	MaxWindowMs    int64
}

// DefaultParams are the production thresholds.
var DefaultParams = Params{
	MaxBitErrors:        10,
	MaxGapFrames:        5,
	MinSpanMs:           15_000,
	MaxIntroMs:          180_000,
	MinChangingFraction: 0.2,
	AgreeMs:             3_000,
	WindowFraction:      0.4,
	MaxWindowMs:         10 * 60_000,
}

// shared is the longest stretch two Prints have in common: [A0, A1) in the first
// and [B0, B1) in the second, in milliseconds on each File's own timeline.
type shared struct {
	A0, A1, B0, B1 int64
}

func (s shared) lengthMs() int64 { return s.A1 - s.A0 }

// longestShared finds the longest run of frames a and b share at any relative
// offset that is not a steady sound (MinChangingFraction). The offset is
// searched exhaustively rather than guessed from hash hits: the prints are short
// (a window of each File, at most a few thousand frames), and an exhaustive
// search cannot miss an alignment because the frames fell a few milliseconds
// apart.
func longestShared(a, b Print, p Params) (shared, bool) {
	na, nb := len(a.Words), len(b.Words)
	changing := changingFrames(a, p)
	bestLen, bestA, bestB := 0, 0, 0
	// j = i - shift: every alignment where at least one frame overlaps.
	for shift := -(nb - 1); shift < na; shift++ {
		i0 := max(0, shift)
		i1 := min(na, nb+shift)
		runStart, lastGood := -1, -1
		closeRun := func() {
			if runStart >= 0 {
				n := lastGood + 1 - runStart
				if n > bestLen && float64(changing[lastGood+1]-changing[runStart]) >= p.MinChangingFraction*float64(n) {
					bestLen, bestA, bestB = n, runStart, runStart-shift
				}
			}
			runStart = -1
		}
		for i := i0; i < i1; i++ {
			j := i - shift
			if a.Voiced[i] && b.Voiced[j] && hamming(a.Words[i], b.Words[j]) <= p.MaxBitErrors {
				if runStart < 0 {
					runStart = i
				}
				lastGood = i
				continue
			}
			if runStart >= 0 && i-lastGood > p.MaxGapFrames {
				closeRun()
			}
		}
		closeRun()
	}
	if bestLen == 0 {
		return shared{}, false
	}
	return shared{
		A0: a.StartMs + frameStartMs(bestA),
		A1: a.StartMs + frameEndMs(bestA+bestLen-1),
		B0: b.StartMs + frameStartMs(bestB),
		B1: b.StartMs + frameEndMs(bestB+bestLen-1),
	}, true
}

// changingFrames is a running count over p's frames of those whose word differs
// from the previous frame's by more than MaxBitErrors: frames [i, j) hold
// c[j]-c[i] of them.
func changingFrames(pr Print, p Params) []int {
	c := make([]int, len(pr.Words)+1)
	for i, w := range pr.Words {
		c[i+1] = c[i]
		if i > 0 && hamming(w, pr.Words[i-1]) > p.MaxBitErrors {
			c[i+1]++
		}
	}
	return c
}

// A frame's word compares frame i with frame i-1, so it describes the audio from
// the start of frame i-1 to the end of frame i. A run of agreeing words therefore
// starts one hop before its first frame and ends a full frame after its last.
func frameStartMs(i int) int64 { return int64(max(i-1, 0)) * FrameMs }
func frameEndMs(i int) int64   { return int64(i)*FrameMs + frameSize*1000/SampleRate }
