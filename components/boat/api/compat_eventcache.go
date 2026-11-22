package main

import (
	clist "container/list"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A finished turn's output never changes, so GET .../events keeps what it read
// from the guest for it. The cache is bounded by entries and by bytes, evicts
// the least recently used, and never holds a turn that might still be writing.
const (
	eventTextCacheEntries = 4096
	eventTextCacheBytes   = 16 << 20
)

// The guest marks a command exited while a child it left behind may still append
// to its stdout. So an entry is not final at once: for eventTextSettle after it
// was first cached it is read again (cmd_logs) when it was last checked at least
// eventTextRecheck ago, and replaced if the log grew. Past the window it is final
// and served from memory with no guest call. Variables so tests can shorten them.
var (
	eventTextSettle  = 10 * time.Second
	eventTextRecheck = 2 * time.Second
	eventTextNow     = time.Now
)

type eventTextKey struct {
	s       *server
	vm, cmd string
}

type eventTextEntry struct {
	key     eventTextKey
	text    string
	size    int64     // the guest log's size when the text was read
	cached  time.Time // first cached
	checked time.Time // last read from the guest
}

type eventTextCache struct {
	mu    sync.Mutex
	ll    *clist.List // front = most recently used
	m     map[eventTextKey]*clist.Element
	bytes int
}

func newEventTextCache() *eventTextCache {
	return &eventTextCache{ll: clist.New(), m: map[eventTextKey]*clist.Element{}}
}

var eventTexts = newEventTextCache()

func (c *eventTextCache) get(k eventTextKey) (string, bool) {
	text, _, ok := c.peek(k)
	return text, ok
}

// peek is get plus whether the entry should be read from the guest again: it is
// inside its settle window and was last checked eventTextRecheck ago or more.
func (c *eventTextCache) peek(k eventTextKey) (text string, recheck, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.m[k]
	if el == nil {
		return "", false, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*eventTextEntry)
	now := eventTextNow()
	return e.text, now.Sub(e.cached) < eventTextSettle && now.Sub(e.checked) >= eventTextRecheck, true
}

func (c *eventTextCache) put(k eventTextKey, text string) {
	c.putSized(k, text, int64(len(text)))
}

// putSized stores text read when the guest log was size bytes long.
func (c *eventTextCache) putSized(k eventTextKey, text string, size int64) {
	if len(text) > eventTextCacheBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := eventTextNow()
	if el := c.m[k]; el != nil {
		// A later read that is longer (the first one raced the guest's last
		// write, or an inherited child kept writing) replaces the shorter one.
		c.ll.MoveToFront(el)
		e := el.Value.(*eventTextEntry)
		e.checked = now
		if size > e.size {
			c.bytes += len(text) - len(e.text)
			e.text, e.size = text, size
			c.evict()
		}
		return
	}
	c.m[k] = c.ll.PushFront(&eventTextEntry{key: k, text: text, size: size, cached: now, checked: now})
	c.bytes += len(text)
	c.evict()
}

// evict drops the least recently used entries until the cache is within its
// bounds (the caller holds mu).
func (c *eventTextCache) evict() {
	for c.ll.Len() > eventTextCacheEntries || c.bytes > eventTextCacheBytes {
		last := c.ll.Back()
		e := c.ll.Remove(last).(*eventTextEntry)
		delete(c.m, e.key)
		c.bytes -= len(e.text)
	}
}

// forget drops every entry of one VM (used when the VM is removed).
func (c *eventTextCache) forget(s *server, vmID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.m {
		if k.s == s && k.vm == vmID {
			e := c.ll.Remove(el).(*eventTextEntry)
			delete(c.m, k)
			c.bytes -= len(e.text)
		}
	}
}

// eventText returns the first compatEventText bytes of a finished turn's
// output: one cmd_logs call, then the cache. A command still running is not
// cached (the caller only asks for turns that ended, but the guest has the last
// word on whether its command stopped). A cached text still inside its settle
// window is read again and replaced when the log grew; if that read fails the
// cached text is served.
func (s *server) eventText(r *http.Request, vmID, cid string) (string, bool) {
	k := eventTextKey{s, vmID, cid}
	cached, recheck, hit := eventTexts.peek(k)
	if hit && !recheck {
		return cached, true
	}
	out, err := s.agentOp(r, vmID, "cmd_logs", map[string]any{"cmd_id": cid, "offset": 0, "limit": compatEventText})
	if err != nil {
		return cached, hit
	}
	b, _ := base64.StdEncoding.DecodeString(str(out, "data_b64"))
	text := strings.ToValidUTF8(string(b), "�")
	if out["running"] == true {
		return text, true
	}
	size := int64(len(b))
	if n, ok := out["size"].(float64); ok && n >= 0 {
		size = int64(n)
	}
	eventTexts.putSized(k, text, size)
	if t, _ := eventTexts.get(k); hit && len(t) >= len(text) {
		return t, true
	}
	return text, true
}
