package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// quickHash hashes the first and last 64 KiB of a file. Two files that share
// size but produce different quick hashes cannot be byte-identical, so they
// can skip the full SHA-256 read.
func quickHash(path string, size int64) (string, error) {
	const window = 64 * 1024
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if size <= int64(window)*2 {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		buf := make([]byte, window)
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
		if _, err := f.Seek(size-int64(window), io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnsureHashes computes a SHA-256 content hash for entries that may be
// duplicates of another entry. It runs in two passes: a quick hash of the
// first and last 64 KiB to prune size collisions that aren't real duplicates,
// then a full SHA-256 only on files that survived the prune. Updates are
// committed in a single transaction.
func (i *Index) EnsureHashes(ctx context.Context, progress func(done, total int)) error {
	type cand struct {
		path string
		size int64
	}

	i.mu.Lock()
	rows, err := i.db.Query(`
		SELECT path, size
		FROM entries
		WHERE (content_hash IS NULL OR content_hash = '')
		  AND size IN (
			  SELECT size FROM entries GROUP BY size HAVING COUNT(*) > 1
		  )
	`)
	if err != nil {
		i.mu.Unlock()
		return err
	}
	var todo []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.path, &c.size); err != nil {
			continue
		}
		todo = append(todo, c)
	}
	rows.Close()
	i.mu.Unlock()

	if progress != nil {
		progress(0, len(todo))
	}
	if len(todo) == 0 {
		return ctx.Err()
	}

	numWorkers := runtime.NumCPU()
	if numWorkers > 8 {
		numWorkers = 8
	}

	// Phase 1: quick-hash every candidate.
	type qres struct {
		path  string
		size  int64
		quick string
		err   error
	}
	jobs := make(chan cand, len(todo))
	for _, t := range todo {
		jobs <- t
	}
	close(jobs)
	qResults := make(chan qres, len(todo))
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
				qResults <- qres{path: t.path, size: t.size, quick: q, err: err}
			}
		}()
	}
	go func() { wg.Wait(); close(qResults) }()

	type key struct {
		size  int64
		quick string
	}
	groups := map[key][]string{}
	done := 0
	for r := range qResults {
		done++
		if progress != nil {
			progress(done, len(todo))
		}
		if r.err != nil {
			continue
		}
		k := key{size: r.size, quick: r.quick}
		groups[k] = append(groups[k], r.path)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Phase 2: only files whose (size, quick) collides with another need a full hash.
	var toFull []string
	for _, paths := range groups {
		if len(paths) >= 2 {
			toFull = append(toFull, paths...)
		}
	}

	fullHashes := make(map[string]string, len(toFull))
	if len(toFull) > 0 {
		if progress != nil {
			progress(0, len(toFull))
		}
		jobs2 := make(chan string, len(toFull))
		for _, p := range toFull {
			jobs2 <- p
		}
		close(jobs2)
		type fres struct {
			path string
			hash string
			err  error
		}
		results2 := make(chan fres, len(toFull))
		var wg2 sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				for path := range jobs2 {
					if ctx.Err() != nil {
						continue
					}
					h, err := hashFile(path)
					results2 <- fres{path: path, hash: h, err: err}
				}
			}()
		}
		go func() { wg2.Wait(); close(results2) }()

		done = 0
		for r := range results2 {
			done++
			if progress != nil {
				progress(done, len(toFull))
			}
			if r.err == nil && ctx.Err() == nil {
				fullHashes[r.path] = r.hash
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Phase 3: commit every full hash in a single transaction.
	if len(fullHashes) > 0 {
		i.mu.Lock()
		defer i.mu.Unlock()
		tx, err := i.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare("UPDATE entries SET content_hash = ? WHERE path = ?")
		if err != nil {
			tx.Rollback()
			return err
		}
		for path, hash := range fullHashes {
			if _, err := stmt.Exec(hash, path); err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return ctx.Err()
}

// FindDuplicates returns groups of entries (size >= 2) that share content
// hash. Within each group, entries are ordered by ModTime ascending (oldest
// first).
func (i *Index) FindDuplicates() []DuplicateGroup {
	i.mu.Lock()
	defer i.mu.Unlock()

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
	i.mu.Lock()
	defer i.mu.Unlock()
	_, err := i.db.Exec("DELETE FROM entries WHERE path = ?", path)
	return err
}
