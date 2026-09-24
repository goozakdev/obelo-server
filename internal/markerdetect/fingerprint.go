package markerdetect

import (
	"math"
	"math/bits"
	"math/cmplx"
)

// The fingerprint (a Haitsma–Kalker style sub-fingerprint, the family Chromaprint
// belongs to). Audio is decoded to mono SampleRate PCM; every hop a frameSize
// window is transformed, its energy is summed into bandCount log-spaced bands,
// and each of the 32 bits says whether the energy difference between two
// neighbouring bands grew or shrank since the previous frame. What the bits
// describe is the SHAPE of the sound changing, not its loudness, so the same
// music reads the same through a different mix level or encoder.
const (
	// SampleRate is what ffmpeg is asked to decode to: mono, 8 kHz. Everything
	// the bands look at is under 4 kHz, and a low rate keeps decoding cheap.
	SampleRate = 8000
	// frameSize is 256 ms of audio per transform (a power of two for the FFT).
	frameSize = 2048
	// hop is the step between frames: 100 ms, so frame i starts at i/10 s.
	hop = 800
	// FrameMs is one frame step in milliseconds — the resolution of a span.
	FrameMs = 1000 * hop / SampleRate
	// bandCount bands give bandCount-1 = 32 bits per frame.
	bandCount  = 33
	bandLowHz  = 300.0
	bandHighHz = 3000.0
	// silenceRMS is the frame loudness (on a 0..1 scale, about -54 dBFS) below
	// which a frame is treated as silence. Silence carries no fingerprint worth
	// matching, and every episode has some, so it must never count as shared.
	silenceRMS = 0.002
)

// Print is the fingerprint of one stretch of a File: one 32-bit word per frame,
// and whether that frame had enough sound to be compared at all. StartMs is where
// the stretch begins on the File's own timeline.
type Print struct {
	StartMs int64
	Words   []uint32
	Voiced  []bool
}

// Fingerprint turns mono PCM at SampleRate into a Print starting at startMs.
func Fingerprint(pcm []int16, startMs int64) Print {
	p := Print{StartMs: startMs}
	if len(pcm) < frameSize {
		return p
	}
	window := hann(frameSize)
	edges := bandEdges()
	buf := make([]complex128, frameSize)
	prev := make([]float64, bandCount)
	cur := make([]float64, bandCount)
	first := true
	for off := 0; off+frameSize <= len(pcm); off += hop {
		var sq float64
		for i := 0; i < frameSize; i++ {
			s := float64(pcm[off+i]) / 32768
			sq += s * s
			buf[i] = complex(s*window[i], 0)
		}
		fft(buf)
		for b := 0; b < bandCount; b++ {
			var e float64
			for k := edges[b]; k < edges[b+1]; k++ {
				e += real(buf[k])*real(buf[k]) + imag(buf[k])*imag(buf[k])
			}
			cur[b] = e
		}
		var w uint32
		if !first {
			for b := 0; b < bandCount-1; b++ {
				if (cur[b]-cur[b+1])-(prev[b]-prev[b+1]) > 0 {
					w |= 1 << b
				}
			}
		}
		p.Words = append(p.Words, w)
		p.Voiced = append(p.Voiced, !first && math.Sqrt(sq/frameSize) >= silenceRMS)
		prev, cur = cur, prev
		first = false
	}
	return p
}

// hamming is how many of the 32 bits two frames disagree on.
func hamming(a, b uint32) int { return bits.OnesCount32(a ^ b) }

func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

// bandEdges returns the FFT bin boundaries of the bandCount log-spaced bands.
func bandEdges() []int {
	edges := make([]int, bandCount+1)
	ratio := math.Pow(bandHighHz/bandLowHz, 1/float64(bandCount))
	for i := range edges {
		hz := bandLowHz * math.Pow(ratio, float64(i))
		edges[i] = int(math.Round(hz * frameSize / SampleRate))
	}
	for i := 1; i < len(edges); i++ {
		if edges[i] <= edges[i-1] {
			edges[i] = edges[i-1] + 1
		}
	}
	return edges
}

// fft is an in-place iterative radix-2 FFT; len(a) must be a power of two.
func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		step := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u := a[start+k]
				v := a[start+k+size/2] * w
				a[start+k] = u + v
				a[start+k+size/2] = u - v
				w *= step
			}
		}
	}
}
