package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
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

// EnsureHashes computes a SHA-256 content hash for entries that don't have
// one yet, restricted to files whose size is shared with at least one other
// entry (a unique-size file cannot be a duplicate). progress is invoked with
// (done, total) where total is the number of files that need hashing.
func (i *Index) EnsureHashes(ctx context.Context, progress func(done, total int)) error {
	type info struct {
		path string
	}

	i.mu.Lock()
	q := `
		SELECT path 
		FROM entries 
		WHERE (content_hash IS NULL OR content_hash = '')
		  AND size IN (
			  SELECT size FROM entries GROUP BY size HAVING COUNT(*) > 1
		  )
	`
	rows, err := i.db.Query(q)
	if err != nil {
		i.mu.Unlock()
		return err
	}
	var todo []info
	for rows.Next() {
		var p info
		if err := rows.Scan(&p.path); err != nil {
			continue
		}
		todo = append(todo, p)
	}
	rows.Close()
	i.mu.Unlock()

	total := len(todo)
	if progress != nil {
		progress(0, total)
	}
	for idx, t := range todo {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, err := hashFile(t.path)
		if err != nil {
			continue
		}
		i.mu.Lock()
		_, _ = i.db.Exec("UPDATE entries SET content_hash = ? WHERE path = ?", h, t.path)
		i.mu.Unlock()
		if progress != nil {
			progress(idx+1, total)
		}
	}
	return nil
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
