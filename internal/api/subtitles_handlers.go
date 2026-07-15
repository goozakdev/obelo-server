package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// Out-of-band text-subtitle delivery (ADR-0020, subtitles/02): a text Subtitle
// track is served as WebVTT from a title-scoped, identity-cacheable endpoint that
// mirrors the artwork media GET —
//
//	GET /api/v1/titles/{id}/subtitles/{subId}.vtt
//
// authenticated by the media cookie OR a bearer (a browser <track> can send
// neither an Authorization header nor a body). The bytes are produced on demand:
// a SIDECAR/Fetched text file is read from disk and converted in-process
// (subtitle.ToWebVTT), an EMBEDDED subtitle Stream is extracted from its
// container by ffmpeg straight to WebVTT. Image tracks (PGS/VOBSUB/DVD) are not
// text and 404 here — they burn in on the transcode tier (a later slice). Text
// selection is entirely client-side, so this endpoint takes no session and never
// mutates state; it's a pure, cacheable read of one track.

// subtitleFFmpegBinary names the ffmpeg executable used to extract an embedded
// subtitle Stream to WebVTT. Empty resolves to "ffmpeg" on PATH, matching
// transcode.FFmpeg / scanner.FFprobe (which the whole app already relies on).
var subtitleFFmpegBinary = ""

// subtitleExtractTimeout bounds the embedded-extraction ffmpeg run. Extracting a
// text subtitle track is near-instant (no decode/re-encode), so a generous cap
// only guards against a pathological/corrupt input hanging the request.
const subtitleExtractTimeout = 30 * time.Second

// subtitleVTTURL is the out-of-band delivery URL for a text track — the value the
// decision's subtitle entry carries and the client fetches into a <track>. It is
// title-scoped (identity-stable, cacheable) and ends in .vtt so a browser treats
// it as a WebVTT resource.
func subtitleVTTURL(titleID, subID string) string {
	return APIPrefix + "/titles/" + titleID + "/subtitles/" + subID + ".vtt"
}

// subtitleOriginalURL is the original-format delivery URL for a text track whose
// source format a capable client (libmpv) prefers raw — same endpoint family as
// the .vtt URL, the extension selecting the delivery format (ADR-0033). format is
// a canonical subtitle.TextFormat token ("srt"|"ass").
func subtitleOriginalURL(titleID, subID, format string) string {
	return APIPrefix + "/titles/" + titleID + "/subtitles/" + subID + "." + format
}

// subtitleContentType maps a delivery format to its response Content-Type. The
// srt/ass types are the conventional ones (neither has an IANA registration that
// players agree on); every consumer we target (libmpv, download tools) keys on
// the URL extension anyway.
func subtitleContentType(format string) string {
	switch format {
	case "srt":
		return "application/x-subrip; charset=utf-8"
	case "ass":
		return "text/x-ssa; charset=utf-8"
	default:
		return "text/vtt; charset=utf-8"
	}
}

// handleTitleSubtitle serves one text Subtitle track of a Title in the delivery
// format the URL extension names (ADR-0020, amended by ADR-0033). It resolves the
// Title under the caller's scope (an out-of-scope / unknown Title is 404, hide
// existence), finds the track by id across the embedded Streams and the
// Sidecar/Fetched rows, and produces the bytes:
//   - format "vtt" → the WebVTT conversion (sidecar/fetched converted in-process,
//     embedded ffmpeg-extracted) — the universal fallback every text track has;
//   - format "srt"/"ass" → the ORIGINAL bytes, styling intact, served only when
//     the track's source format matches (sidecar/fetched read raw off disk,
//     embedded codec-copied out of the container by ffmpeg);
//   - image (either source), an unknown id, an unconvertible text format, or a
//     format mismatch (asking .ass of an srt track) → 404.
//
// The response is cacheable (deterministic for a given track) and typed per
// format (text/vtt, application/x-subrip, text/x-ssa).
func handleTitleSubtitle(cat *catalog.Service, titleID, subID, format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if titleID == "" || subID == "" || strings.Contains(subID, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		if _, ok := identityFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		d, err := cat.GetTitle(scope, titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "subtitle not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load subtitle", nil)
			return
		}

		var data []byte
		if format == "vtt" {
			data, err = subtitleVTT(r.Context(), d, subID)
		} else {
			data, err = subtitleOriginal(r.Context(), d, subID, format)
		}
		switch {
		case errors.Is(err, errSubtitleNotText), errors.Is(err, errSubtitleNotFound):
			// An unknown id, an image track, an unconvertible text format, or a
			// format mismatch are all "no such subtitle here" — hide the distinction
			// behind a 404.
			writeError(w, http.StatusNotFound, codeNotFound, "subtitle not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to render subtitle", nil)
			return
		}

		w.Header().Set("Content-Type", subtitleContentType(format))
		// The bytes are deterministic for a given track (path/mtime don't change
		// without a rescan), so let the client cache the fetch.
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// errSubtitleNotFound means no track with the given id belongs to the Title.
// errSubtitleNotText means the track exists but isn't a deliverable text track
// (an image track, or a text format ToWebVTT doesn't handle). Both surface as 404.
var (
	errSubtitleNotFound = errors.New("api: subtitle track not found")
	errSubtitleNotText  = errors.New("api: subtitle track is not deliverable text")
)

// subtitleVTT locates the track subID within the Title detail and returns its
// WebVTT bytes. It checks the Sidecar/Fetched rows and the embedded subtitle
// Streams (a Stream id vs a subtitle-row id are distinct id spaces, so at most one
// matches). ctx bounds any ffmpeg extraction.
func subtitleVTT(ctx context.Context, d store.TitleDetail, subID string) ([]byte, error) {
	// Sidecar/Fetched rows: a text file on disk, converted in-process.
	for _, sub := range d.Subtitles {
		if sub.ID != subID {
			continue
		}
		if sub.Kind != "text" || !subtitle.IsTextConvertible(sub.Codec) {
			return nil, errSubtitleNotText
		}
		data, err := os.ReadFile(sub.Path)
		if err != nil {
			return nil, err
		}
		return subtitle.ToWebVTT(data, sub.Codec)
	}
	// Embedded Streams: extracted from the container by ffmpeg.
	for _, ed := range d.Editions {
		for _, f := range ed.Files {
			for _, s := range f.Streams {
				if s.ID != subID || s.Kind != "subtitle" {
					continue
				}
				if subtitle.KindForCodec(s.Codec) != "text" {
					return nil, errSubtitleNotText
				}
				if !f.Present {
					// The row survives a soft-delete but the bytes are gone.
					return nil, errSubtitleNotFound
				}
				return extractEmbeddedVTT(ctx, f.Path, s.Index)
			}
		}
	}
	return nil, errSubtitleNotFound
}

// subtitleOriginal locates the track subID within the Title detail and returns its
// ORIGINAL bytes in the requested format (ADR-0033) — styling intact for clients
// (libmpv) that render srt/ass natively. Unlike the WebVTT path there is no
// conversion: the track's own format must MATCH the request (subtitle.TextFormat
// folds aliases: subrip→srt, ssa→ass), else errSubtitleNotText — asking .ass of an
// srt track is "no such subtitle here", not a transcode.
func subtitleOriginal(ctx context.Context, d store.TitleDetail, subID, format string) ([]byte, error) {
	// Sidecar/Fetched rows: the original file is on disk — serve it raw.
	for _, sub := range d.Subtitles {
		if sub.ID != subID {
			continue
		}
		if sub.Kind != "text" || subtitle.TextFormat(sub.Codec) != format {
			return nil, errSubtitleNotText
		}
		return os.ReadFile(sub.Path)
	}
	// Embedded Streams: codec-copied out of the container by ffmpeg (no transcode,
	// so ASS styling survives). mov_text folds to "" and never matches here — it
	// stays WebVTT-only.
	for _, ed := range d.Editions {
		for _, f := range ed.Files {
			for _, s := range f.Streams {
				if s.ID != subID || s.Kind != "subtitle" {
					continue
				}
				if subtitle.KindForCodec(s.Codec) != "text" || subtitle.TextFormat(s.Codec) != format {
					return nil, errSubtitleNotText
				}
				if !f.Present {
					return nil, errSubtitleNotFound
				}
				return extractEmbeddedOriginal(ctx, f.Path, s.Index, format)
			}
		}
	}
	return nil, errSubtitleNotFound
}

// extractEmbeddedVTT runs ffmpeg to pull one subtitle Stream (by its absolute
// container index) out of a File and emit it as WebVTT on stdout. Mapping only
// that stream keeps the WebVTT muxer happy (it carries no video/audio), and the
// webvtt encoder downgrades any text codec (mov_text/subrip/ass) to plain cues —
// the same styling downgrade the sidecar path makes. A non-zero exit / no output
// is an error the caller renders as a 500 (the track was advertised, so this is a
// genuine server-side failure, not a 404).
func extractEmbeddedVTT(ctx context.Context, path string, index int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, subtitleExtractTimeout)
	defer cancel()

	bin := subtitleFFmpegBinary
	if bin == "" {
		bin = "ffmpeg"
	}
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin,
		"-loglevel", "error",
		"-i", path,
		"-map", "0:"+strconv.Itoa(index),
		"-f", "webvtt",
		"-", // stdout
	)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// Surface ffmpeg's stderr (loglevel error) in the wrapped error so a failed
		// extraction is diagnosable server-side; the client still gets a plain 500.
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return nil, fmt.Errorf("api: ffmpeg subtitle extraction failed: %s: %w", msg, err)
		}
		return nil, err
	}
	if out.Len() == 0 {
		return nil, errors.New("api: ffmpeg produced no WebVTT output")
	}
	return out.Bytes(), nil
}

// extractEmbeddedOriginal runs ffmpeg to pull one subtitle Stream out of a File in
// its ORIGINAL codec — `-c:s copy` into the matching muxer (srt or ass), so no
// styling is lost (the whole point of ADR-0033's original-format delivery). Same
// timeout/error posture as extractEmbeddedVTT.
func extractEmbeddedOriginal(ctx context.Context, path string, index int, format string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, subtitleExtractTimeout)
	defer cancel()

	bin := subtitleFFmpegBinary
	if bin == "" {
		bin = "ffmpeg"
	}
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin,
		"-loglevel", "error",
		"-i", path,
		"-map", "0:"+strconv.Itoa(index),
		"-c:s", "copy",
		"-f", format, // "srt" and "ass" are both ffmpeg muxer names
		"-", // stdout
	)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return nil, fmt.Errorf("api: ffmpeg original-subtitle extraction failed: %s: %w", msg, err)
		}
		return nil, err
	}
	if out.Len() == 0 {
		return nil, errors.New("api: ffmpeg produced no subtitle output")
	}
	return out.Bytes(), nil
}

// dispatchTitleSubtitle routes GET {id}/subtitles/{subId}.{ext} off the title
// subtree — .vtt (the universal WebVTT conversion) plus the original-format
// extensions .srt and .ass (ADR-0033). It is a media GET (cookie-or-bearer) like
// the artwork leaf. The subId is the path element before the extension and must
// be a single element; an unrecognized extension is not this route's (→ the
// subtree's 404).
func dispatchTitleSubtitle(deps Deps, rest string) (http.HandlerFunc, bool) {
	i := strings.Index(rest, "/subtitles/")
	if i <= 0 {
		return nil, false
	}
	titleID := rest[:i]
	tail := rest[i+len("/subtitles/"):]
	var subID, format string
	for _, ext := range []string{".vtt", ".srt", ".ass"} {
		if id, ok := strings.CutSuffix(tail, ext); ok {
			subID, format = id, ext[1:]
			break
		}
	}
	if format == "" || subID == "" || strings.Contains(subID, "/") || strings.Contains(titleID, "/") {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		}, true
	}
	return requireMethod(http.MethodGet,
		requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleTitleSubtitle(deps.Catalog, titleID, subID, format)))), true
}
