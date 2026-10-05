package api

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/subtitle"
)

// subtitleCues is a track's whole-file WebVTT parsed once for repeated segment
// requests.
type subtitleCues = subtitle.SegmentCues

// parseSegmentCues is the parse step, a var so a test can count calls.
var parseSegmentCues = subtitle.ParseSegmentCues

// subtitleCueCacheMax bounds the parsed-cue cache. Each entry is one track of one
// live session, so this comfortably covers concurrent sessions while keeping a
// long-lived server from accumulating entries for sessions that have ended.
const subtitleCueCacheMax = 64

// subtitleCueStamp identifies the whole-file cache file a cue set was parsed
// from. A new extraction (the session's scratch dir was removed and recreated, or
// the file was rewritten) changes it, which invalidates the entry.
type subtitleCueStamp struct {
	size int64
	mod  time.Time
}

type subtitleCueEntry struct {
	key   string
	stamp subtitleCueStamp
	cues  subtitleCues
}

// subtitleCueCache is a small LRU of parsed cue sets keyed by cache-file path.
type subtitleCueCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List // front = most recent; values are *subtitleCueEntry
	m   map[string]*list.Element
}

func newSubtitleCueCache(max int) *subtitleCueCache {
	return &subtitleCueCache{max: max, ll: list.New(), m: map[string]*list.Element{}}
}

var segmentCueCache = newSubtitleCueCache(subtitleCueCacheMax)

func (c *subtitleCueCache) get(key string, stamp subtitleCueStamp) (subtitleCues, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return subtitleCues{}, false
	}
	e := el.Value.(*subtitleCueEntry)
	if e.stamp != stamp {
		c.ll.Remove(el)
		delete(c.m, key)
		return subtitleCues{}, false
	}
	c.ll.MoveToFront(el)
	return e.cues, true
}

func (c *subtitleCueCache) put(key string, stamp subtitleCueStamp, cues subtitleCues) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		e := el.Value.(*subtitleCueEntry)
		e.stamp, e.cues = stamp, cues
		c.ll.MoveToFront(el)
		return
	}
	c.m[key] = c.ll.PushFront(&subtitleCueEntry{key: key, stamp: stamp, cues: cues})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*subtitleCueEntry).key)
	}
}

func (c *subtitleCueCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// subtitleCachePath is a session's whole-file WebVTT cache file for one track.
func subtitleCachePath(scratchDir, subID string) string {
	return filepath.Join(scratchDir, "subfull_"+subID+".vtt")
}

// subtitleSegmentCues returns a track's parsed cues for segment slicing. The parse
// is cached against the session's whole-file cache file, so the WebVTT is split
// and parsed once per (session, track) instead of once per segment request. With
// no scratch dir (defensive) nothing is cached.
func subtitleSegmentCues(ctx context.Context, sctx playback.SessionSubtitleContext, subID string) (subtitleCues, error) {
	full, err := wholeSubtitleVTT(ctx, sctx, subID)
	if err != nil {
		return subtitleCues{}, err
	}
	if sctx.ScratchDir == "" {
		return parseSegmentCues(full), nil
	}
	key := subtitleCachePath(sctx.ScratchDir, subID)
	var stamp subtitleCueStamp
	if fi, err := os.Stat(key); err == nil {
		stamp = subtitleCueStamp{size: fi.Size(), mod: fi.ModTime()}
		if cues, ok := segmentCueCache.get(key, stamp); ok {
			return cues, nil
		}
	}
	cues := parseSegmentCues(full)
	if !stamp.mod.IsZero() {
		segmentCueCache.put(key, stamp, cues)
	}
	return cues, nil
}

// subtitleFlight collapses concurrent calls for one key into a single run of fn;
// the others wait and share its result. The run is detached from any one caller's
// cancellation (a follower must not inherit the first caller's abort) but is
// cancelled once no waiter remains, and every waiter returns on its own ctx.
type subtitleFlight struct {
	mu    sync.Mutex
	calls map[string]*subtitleCall
}

type subtitleCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	data    []byte
	err     error
}

var wholeSubtitleFlight = &subtitleFlight{calls: map[string]*subtitleCall{}}

func (f *subtitleFlight) do(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	c, ok := f.calls[key]
	if ok {
		c.waiters++
	} else {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c = &subtitleCall{done: make(chan struct{}), cancel: cancel, waiters: 1}
		f.calls[key] = c
		go f.run(runCtx, key, c, fn)
	}
	f.mu.Unlock()

	select {
	case <-c.done:
		return c.data, c.err
	case <-ctx.Done():
		f.mu.Lock()
		c.waiters--
		if c.waiters == 0 {
			// Nobody wants the result: stop the work, and make new callers
			// start a fresh run rather than join a cancelled one.
			c.cancel()
			if f.calls[key] == c {
				delete(f.calls, key)
			}
		}
		f.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (f *subtitleFlight) run(ctx context.Context, key string, c *subtitleCall, fn func(context.Context) ([]byte, error)) {
	defer func() {
		if r := recover(); r != nil {
			c.data, c.err = nil, fmt.Errorf("api: subtitle extraction panicked: %v", r)
		}
		f.mu.Lock()
		if f.calls[key] == c {
			delete(f.calls, key)
		}
		f.mu.Unlock()
		c.cancel()
		close(c.done)
	}()
	c.data, c.err = fn(ctx)
}
