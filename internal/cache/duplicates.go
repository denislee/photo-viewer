package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

// DuplicateGroup is a set of entries that share identical content.
type DuplicateGroup struct {
	Hash    string
	Entries []Entry
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// quickHashWindow is the read-window size in bytes for quickHash. 64 KiB is
// chosen so two reads cover the head and tail of a typical media file without
// pulling in arbitrarily large blocks.
const quickHashWindow = 64 * 1024

// quickHashBufPool recycles 64 KiB read buffers across quickHash calls so the
// duplicate scan doesn't burn an allocation per file.
var quickHashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, quickHashWindow)
		return &b
	},
}

// quickHash hashes the first and last 64 KiB of a file. Two files that share
// size but produce different quick hashes cannot be byte-identical, so they
// can skip the full SHA-256 read.
func quickHash(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if size <= int64(quickHashWindow)*2 {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		bufp := quickHashBufPool.Get().(*[]byte)
		defer quickHashBufPool.Put(bufp)
		buf := *bufp
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
		if _, err := f.Seek(size-int64(quickHashWindow), io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashBatchSize is how many computed hashes to accumulate before committing
// to SQLite. Smaller = more granular resume on cancel, but more transactions.
const hashBatchSize = 500

// EnsureHashes fills in missing content hashes for entries that share a size
// with at least one other entry. It runs in two passes: a quick hash of the
// first and last 64 KiB to prune size collisions, then a full SHA-256 only on
// files that survived the prune.
//
// Both the quick hash and the full hash are persisted to the index, and
// commits happen in batches throughout the run — so cancelling part way
// through preserves progress and the next run only processes whatever is
// still missing.
func (i *Index) EnsureHashes(ctx context.Context, progress func(done, total int)) error {
	type cand struct {
		path  string
		size  int64
		quick string // already-cached quick hash, empty if not computed yet
		full  string // already-cached full hash, empty if not computed yet
	}

	rows, err := i.db.Query(`
		SELECT path, size, COALESCE(quick_hash, ''), COALESCE(content_hash, '')
		FROM entries
		WHERE size IN (
			SELECT size FROM entries GROUP BY size HAVING COUNT(*) > 1
		)
	`)
	if err != nil {
		return err
	}
	var todo []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.path, &c.size, &c.quick, &c.full); err != nil {
			continue
		}
		todo = append(todo, c)
	}
	rows.Close()

	if len(todo) == 0 {
		if progress != nil {
			progress(0, 0)
		}
		return ctx.Err()
	}

	numWorkers := runtime.NumCPU()
	if numWorkers < 4 {
		numWorkers = 4
	}

	// Phase 1: quick-hash candidates that don't have a cached quick hash.
	var phase1 []cand
	for _, c := range todo {
		if c.quick == "" {
			phase1 = append(phase1, c)
		}
	}

	// Track quick hashes (cached + freshly computed).
	quicks := make(map[string]string, len(todo))
	for _, c := range todo {
		if c.quick != "" {
			quicks[c.path] = c.quick
		}
	}

	if progress != nil {
		progress(len(todo)-len(phase1), len(todo))
	}

	if len(phase1) > 0 {
		type qres struct {
			path  string
			quick string
		}
		jobs := make(chan cand, numWorkers*2)
		go func() {
			defer close(jobs)
			for _, t := range phase1 {
				select {
				case <-ctx.Done():
					return
				case jobs <- t:
				}
			}
		}()
		results := make(chan qres, numWorkers*2)
		var wg sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range jobs {
					if ctx.Err() != nil {
						continue
					}
					q, err := quickHash(t.path, t.size)
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							_ = i.RemoveEntry(t.path)
						}
						continue
					}
					results <- qres{path: t.path, quick: q}
				}
			}()
		}
		go func() { wg.Wait(); close(results) }()

		pending := make([]qres, 0, hashBatchSize)
		flush := func() error {
			if len(pending) == 0 {
				return nil
			}
			tx, err := i.db.Begin()
			if err != nil {
				return err
			}
			stmt, err := tx.Prepare("UPDATE entries SET quick_hash = ? WHERE path = ?")
			if err != nil {
				tx.Rollback()
				return err
			}
			for _, r := range pending {
				_, _ = stmt.Exec(r.quick, r.path)
			}
			stmt.Close()
			err = tx.Commit()
			pending = pending[:0]
			return err
		}

		done := len(todo) - len(phase1)
		for r := range results {
			quicks[r.path] = r.quick
			pending = append(pending, r)
			done++
			if progress != nil {
				progress(done, len(todo))
			}
			if len(pending) >= hashBatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// Build (size, quick) groups using all cached + computed quick hashes.
	type key struct {
		size  int64
		quick string
	}
	groups := map[key][]cand{}
	for _, c := range todo {
		q := c.quick
		if q == "" {
			q = quicks[c.path]
		}
		if q == "" {
			continue // quick hash failed earlier
		}
		groups[key{size: c.size, quick: q}] = append(groups[key{size: c.size, quick: q}],
			cand{path: c.path, size: c.size, quick: q, full: c.full})
	}

	// Phase 2: full-hash files whose (size, quick) collides with another and
	// don't already have a cached content hash.
	var toFull []cand
	for _, gs := range groups {
		if len(gs) < 2 {
			continue
		}
		for _, c := range gs {
			if c.full == "" {
				toFull = append(toFull, c)
			}
		}
	}

	if len(toFull) == 0 {
		return ctx.Err()
	}

	if progress != nil {
		progress(0, len(toFull))
	}

	type fres struct {
		path string
		hash string
	}
	jobs2 := make(chan cand, numWorkers*2)
	go func() {
		defer close(jobs2)
		for _, c := range toFull {
			select {
			case <-ctx.Done():
				return
			case jobs2 <- c:
			}
		}
	}()
	results2 := make(chan fres, numWorkers*2)
	var wg2 sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for c := range jobs2 {
				if ctx.Err() != nil {
					continue
				}
				h, err := hashFile(c.path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						_ = i.RemoveEntry(c.path)
					}
					continue
				}
				results2 <- fres{path: c.path, hash: h}
			}
		}()
	}
	go func() { wg2.Wait(); close(results2) }()

	pending := make([]fres, 0, hashBatchSize)
	flushFull := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := i.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare("UPDATE entries SET content_hash = ? WHERE path = ?")
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, r := range pending {
			_, _ = stmt.Exec(r.hash, r.path)
		}
		stmt.Close()
		err = tx.Commit()
		pending = pending[:0]
		return err
	}

	done := 0
	for r := range results2 {
		pending = append(pending, r)
		done++
		if progress != nil {
			progress(done, len(toFull))
		}
		if len(pending) >= hashBatchSize {
			if err := flushFull(); err != nil {
				return err
			}
		}
	}
	if err := flushFull(); err != nil {
		return err
	}

	return ctx.Err()
}

// FindDuplicates returns groups of entries (size >= 2) that share content
// hash. Within each group, entries are ordered by ModTime ascending (oldest
// first).
func (i *Index) FindDuplicates() []DuplicateGroup {

	rows, err := i.db.Query(`
		SELECT path, type, size, mtime, thumb_id, content_hash
		FROM entries
		WHERE content_hash IS NOT NULL AND content_hash != ''
		AND content_hash IN (
			SELECT content_hash FROM entries
			WHERE content_hash IS NOT NULL AND content_hash != ''
			GROUP BY content_hash HAVING COUNT(*) > 1
		)
		ORDER BY content_hash, mtime ASC, path ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	groupsByHash := map[string]*DuplicateGroup{}
	var order []string
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var hash string
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &hash); err != nil {
			continue
		}
		e.ModTime = time.Unix(mtimeUnix, 0)
		g, ok := groupsByHash[hash]
		if !ok {
			g = &DuplicateGroup{Hash: hash}
			groupsByHash[hash] = g
			order = append(order, hash)
		}
		g.Entries = append(g.Entries, e)
	}

	out := make([]DuplicateGroup, 0, len(order))
	for _, h := range order {
		out = append(out, *groupsByHash[h])
	}
	return out
}

// RemoveEntry deletes a single row from the index by path.
func (i *Index) RemoveEntry(path string) error {
	_, err := i.db.Exec("DELETE FROM entries WHERE path = ?", path)
	return err
}
