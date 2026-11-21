package webserver

import (
	"container/list"
	"sync"

	"github.com/dns/photo-viewer/internal/scan"
)

// infoCacheCap bounds the /api/info metadata cache. An entry is a handful of
// short strings, so a few hundred cover a long browsing session in well under
// a megabyte.
const infoCacheCap = 512

// mediaInfoFn is the exiftool lookup /api/info uses. A package var only so
// tests can count calls without an exiftool binary; production never
// reassigns it.
var mediaInfoFn = scan.GetMediaInfo

// infoVal is the expensive part of an /api/info response: the exiftool
// metadata and the decoded dimensions. Favorite and the other index fields
// are read fresh on every request.
type infoVal struct {
	mi   scan.MediaInfo
	dims string
}

type infoItem struct {
	key fileVersion
	val infoVal
}

// infoCache is a small LRU over infoVal. The zero value is ready to use.
// Entries are keyed by file version, so no TTL is needed: an edited file gets a
// new key and the old one ages out of the LRU.
type infoCache struct {
	mu sync.Mutex
	ll *list.List // front = most recently used; elements hold *infoItem
	m  map[fileVersion]*list.Element
}

func (c *infoCache) get(k fileVersion) (infoVal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		return infoVal{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*infoItem).val, true
}

func (c *infoCache) put(k fileVersion, v infoVal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[fileVersion]*list.Element)
		c.ll = list.New()
	}
	if el, ok := c.m[k]; ok {
		el.Value.(*infoItem).val = v
		c.ll.MoveToFront(el)
		return
	}
	c.m[k] = c.ll.PushFront(&infoItem{key: k, val: v})
	if c.ll.Len() > infoCacheCap {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.m, oldest.Value.(*infoItem).key)
	}
}
