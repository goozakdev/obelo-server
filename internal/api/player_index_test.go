package api_test

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// ADR-0067: a Decision's Streams carry `playerIndex` — the index a player that
// opens the HLS playlist sees — on HLS Decisions, not directPlay. `index` keeps its
// container meaning. These tests run against the multi-audio fixture (Audio Movie:
// one video + four audio Streams) through the real HTTP + ffmpeg stack, and hold the
// number to the generated master playlist itself, not to the rule that computes it.

type playerIndexStream struct {
	ID          string `json:"id"`
	Index       int    `json:"index"`
	PlayerIndex *int   `json:"playerIndex"`
}

type playerIndexDecision struct {
	Tier         string              `json:"tier"`
	StreamURL    string              `json:"streamUrl"`
	VideoStream  *playerIndexStream  `json:"videoStream"`
	AudioStream  *playerIndexStream  `json:"audioStream"`
	AudioStreams []playerIndexStream `json:"audioStreams"`
	VideoStreams []playerIndexStream `json:"videoStreams"`
}

func negotiatePlayerIndex(t *testing.T, profile map[string]any) (playerIndexDecision, []byte) {
	t.Helper()
	srv, token, id := scanAudioMovieLib(t)
	var dec playerIndexDecision
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	return dec, raw
}

func TestDemuxedDecisionReportsPlayerIndexesMatchingTheMasterOrder(t *testing.T) {
	t.Parallel()
	requireAudioFixtures(t)
	srv, token, id := scanAudioMovieLib(t)
	var dec playerIndexDecision
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, remuxMultiAudioProfile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	if dec.Tier != "directStream" || !strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("tier/streamUrl = %s %s, want a demuxed directStream master", dec.Tier, dec.StreamURL)
	}

	// Golden: the master's own order. Each AUDIO rendition's position among the
	// #EXT-X-MEDIA lines is the index its Stream must report, matched by the stream id
	// in the rendition URI (audio_<streamId>.m3u8).
	master := fetchText(t, srv, dec.StreamURL, token)
	masterOrder := map[string]int{}
	renditions := 0
	for _, line := range strings.Split(master, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") {
			i := strings.Index(line, `URI="audio_`)
			if i < 0 {
				t.Fatalf("audio line without an audio_ URI: %s", line)
			}
			rest := line[i+len(`URI="audio_`):]
			masterOrder[rest[:strings.Index(rest, `.m3u8"`)]] = renditions
		}
		renditions++
	}
	if len(masterOrder) != 4 || len(dec.AudioStreams) != 4 {
		t.Fatalf("audio renditions = %d, decision audioStreams = %d, want 4 and 4\n%s", len(masterOrder), len(dec.AudioStreams), master)
	}
	for _, a := range dec.AudioStreams {
		want, ok := masterOrder[a.ID]
		if !ok {
			t.Fatalf("audio stream %s has no rendition in the master:\n%s", a.ID, master)
		}
		if a.PlayerIndex == nil || *a.PlayerIndex != want {
			t.Errorf("audio %s playerIndex = %v, want %d (its place in the master)", a.ID, a.PlayerIndex, want)
		}
		// index keeps its container meaning: it is never rewritten to the player's.
		if a.Index < 1 {
			t.Errorf("audio %s index = %d, want its container index (>= 1; video is container index 0)", a.ID, a.Index)
		}
	}
	// The video is the variant, after every rendition.
	if len(dec.VideoStreams) != 1 || dec.VideoStreams[0].PlayerIndex == nil || *dec.VideoStreams[0].PlayerIndex != renditions {
		t.Errorf("videoStreams = %+v, want one Stream with playerIndex %d", dec.VideoStreams, renditions)
	}
	if dec.VideoStream == nil || dec.VideoStream.PlayerIndex == nil || *dec.VideoStream.PlayerIndex != renditions || dec.VideoStream.Index != 0 {
		t.Errorf("videoStream = %+v, want index 0, playerIndex %d", dec.VideoStream, renditions)
	}
	if dec.AudioStream == nil || dec.AudioStream.PlayerIndex == nil {
		t.Fatalf("resolved audioStream = %+v, want a playerIndex", dec.AudioStream)
	}
	if want := masterOrder[audioIDByIndex(t, dec, dec.AudioStream.Index)]; *dec.AudioStream.PlayerIndex != want {
		t.Errorf("resolved audioStream playerIndex = %d, want %d", *dec.AudioStream.PlayerIndex, want)
	}

	// Probe: a real demuxer reading the master numbers its streams the same way
	// (audio renditions in order, then the video). This is what mpv joins on.
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH; golden master-order assertions above ran")
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-headers", "Authorization: Bearer "+token+"\r\n",
		"-print_format", "json", "-show_streams", srv.URL(dec.StreamURL)).Output()
	if err != nil {
		t.Fatalf("ffprobe master: %v", err)
	}
	var probe struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parsing ffprobe json: %v\n%s", err, out)
	}
	if len(probe.Streams) != 5 {
		t.Fatalf("ffprobe saw %d streams, want 5 (4 audio + 1 video): %s", len(probe.Streams), out)
	}
	for _, a := range dec.AudioStreams {
		s := probe.Streams[*a.PlayerIndex]
		if s.Index != *a.PlayerIndex || s.CodecType != "audio" {
			t.Errorf("ffprobe stream %d = %+v, want audio at that index (audio %s)", *a.PlayerIndex, s, a.ID)
		}
	}
	v := probe.Streams[*dec.VideoStream.PlayerIndex]
	if v.CodecType != "video" || v.Index != *dec.VideoStream.PlayerIndex {
		t.Errorf("ffprobe stream at the video playerIndex = %+v, want video", v)
	}
}

func audioIDByIndex(t *testing.T, dec playerIndexDecision, index int) string {
	t.Helper()
	for _, a := range dec.AudioStreams {
		if a.Index == index {
			return a.ID
		}
	}
	t.Fatalf("no audioStreams entry with container index %d: %+v", index, dec.AudioStreams)
	return ""
}

func TestDecisionOmitsPlayerIndexWhereThePlayerSeesTheContainer(t *testing.T) {
	t.Parallel()
	requireAudioFixtures(t)
	// directPlay: the player opens the file itself, so index already is its index.
	dec, raw := negotiatePlayerIndex(t, mkvMultiAudioProfile())
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %s, want directPlay", dec.Tier)
	}
	if strings.Contains(string(raw), "playerIndex") {
		t.Errorf("a directPlay Decision carries playerIndex: %s", raw)
	}
}
