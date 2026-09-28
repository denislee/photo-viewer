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

// infoKey identifies one version of a file. A file's metadata can only change
// if its bytes do, which moves mtime/size, so no TTL is needed: an edited
// file gets a new key and the old one ages out of the LRU.
type infoKey struct {
	id    string
	mtime int64
	size  int64
}

// infoVal is the expensive part of an /api/info response: the exiftool
// metadata and the decoded dimensions. Favorite and the other index fields
// are read fresh on every request.
type infoVal struct {
	mi   scan.MediaInfo
	dims string
}

type infoItem struct {
	key infoKey
	val infoVal
}

// infoCache is a small LRU over infoVal. The zero value is ready to use.
type infoCache struct {
	mu sync.Mutex
	ll *list.List // front = most recently used; elements hold *infoItem
	m  map[infoKey]*list.Element
}

func (c *infoCache) get(k infoKey) (infoVal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		return infoVal{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*infoItem).val, true
}

func (c *infoCache) put(k infoKey, v infoVal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[infoKey]*list.Element)
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
