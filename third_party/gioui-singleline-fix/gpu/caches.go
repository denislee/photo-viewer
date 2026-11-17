// SPDX-License-Identifier: Unlicense OR MIT

package gpu

import (
	"fmt"
	"image"
	"sort"
)

type textureCacheKey struct {
	filter byte
	handle any
}

type textureCache struct {
	res map[textureCacheKey]resourceCacheValue
	// frameNum counts frame() calls. Local patch (texture-retention.patch):
	// upstream releases a texture the first frame it goes unused, so closing
	// a full-screen view re-uploads every texture behind it in one frame, and
	// scrolling back re-uploads what just left the screen.
	frameNum uint64
}

type resourceCacheValue struct {
	used     bool
	lastUsed uint64 // frameNum of the last frame the resource was used in
	resource resource
}

// Unused textures are kept for up to textureKeepFrames frames, and only while
// the unused ones together stay within textureKeepBytes of pixel data; past
// that the least recently used are released first.
const (
	textureKeepFrames = 120
	textureKeepBytes  = 128 << 20
)

// opCache is like a resourceCache but using concrete types and a
// freelist instead of two maps to avoid runtime.mapaccess2 calls
// since benchmarking showed them as a bottleneck.
type opCache struct {
	// store the index + 1 in cache this key is stored in
	index map[opKey]int
	// list of indexes in cache that are free and can be used
	freelist []int
	cache    []opCacheValue
}

type opCacheValue struct {
	data pathData

	bounds image.Rectangle
	// the fields below are handled by opCache
	key  opKey
	keep bool
}

func newTextureCache() *textureCache {
	return &textureCache{
		res: make(map[textureCacheKey]resourceCacheValue),
	}
}

func (r *textureCache) get(key textureCacheKey) (resource, bool) {
	v, exists := r.res[key]
	if !exists {
		return nil, false
	}
	if !v.used {
		v.used = true
		r.res[key] = v
	}
	return v.resource, exists
}

func (r *textureCache) put(key textureCacheKey, val resource) {
	v, exists := r.res[key]
	if exists && v.used {
		panic(fmt.Errorf("key exists, %v", key))
	}
	v.used = true
	v.resource = val
	r.res[key] = v
}

func (r *textureCache) frame() {
	r.frameNum++
	type idle struct {
		key      textureCacheKey
		lastUsed uint64
		bytes    int
	}
	var idles []idle
	idleBytes := 0
	for k, v := range r.res {
		switch {
		case v.used:
			v.used = false
			v.lastUsed = r.frameNum
			r.res[k] = v
		case r.frameNum-v.lastUsed > textureKeepFrames:
			delete(r.res, k)
			v.resource.release()
		default:
			b := resourceBytes(v.resource)
			idleBytes += b
			idles = append(idles, idle{k, v.lastUsed, b})
		}
	}
	if idleBytes <= textureKeepBytes {
		return
	}
	sort.Slice(idles, func(i, j int) bool { return idles[i].lastUsed < idles[j].lastUsed })
	for _, it := range idles {
		if idleBytes <= textureKeepBytes {
			break
		}
		v := r.res[it.key]
		delete(r.res, it.key)
		v.resource.release()
		idleBytes -= it.bytes
	}
}

// resourceBytes estimates a cached resource's pixel memory.
func resourceBytes(res resource) int {
	if t, ok := res.(*texture); ok && t.src != nil {
		return 4 * t.src.Bounds().Dx() * t.src.Bounds().Dy()
	}
	return 0
}

func (r *textureCache) release() {
	for _, v := range r.res {
		v.resource.release()
	}
	r.res = nil
}

func newOpCache() *opCache {
	return &opCache{
		index:    make(map[opKey]int),
		freelist: make([]int, 0),
		cache:    make([]opCacheValue, 0),
	}
}

func (r *opCache) get(key opKey) (o opCacheValue, exist bool) {
	v := r.index[key]
	if v == 0 {
		return
	}
	r.cache[v-1].keep = true
	return r.cache[v-1], true
}

func (r *opCache) put(key opKey, val opCacheValue) {
	v := r.index[key]
	val.keep = true
	val.key = key
	if v == 0 {
		// not in cache
		i := len(r.cache)
		if len(r.freelist) > 0 {
			i = r.freelist[len(r.freelist)-1]
			r.freelist = r.freelist[:len(r.freelist)-1]
			r.cache[i] = val
		} else {
			r.cache = append(r.cache, val)
		}
		r.index[key] = i + 1
	} else {
		r.cache[v-1] = val
	}
}

func (r *opCache) frame() {
	r.freelist = r.freelist[:0]
	for i, v := range r.cache {
		r.cache[i].keep = false
		if v.keep {
			continue
		}
		if v.data.data != nil {
			v.data.release()
			r.cache[i].data.data = nil
		}
		delete(r.index, v.key)
		r.freelist = append(r.freelist, i)
	}
}

func (r *opCache) release() {
	for i := range r.cache {
		r.cache[i].keep = false
	}
	r.frame()
	r.index = nil
	r.freelist = nil
	r.cache = nil
}
