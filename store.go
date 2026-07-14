package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Post is one cat photo with its caption. Everything lives in posts.json;
// the images sit next to it on disk. Backing the site up is `cp -r data`.
type Post struct {
	ID       string    `json:"id"`       // slug, also the permalink
	Date     time.Time `json:"date"`     // the day it happened, shown on the page
	Caption  string    `json:"caption"`  // plain text, never HTML
	Image    string    `json:"image"`    // display JPEG in media/
	Thumb    string    `json:"thumb"`    // small JPEG in media/, used by the archive + feed
	Original string    `json:"original"` // untouched upload in originals/, never served
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Bytes    int64     `json:"bytes"` // size of Image, for the RSS enclosure
	Created  time.Time `json:"created"`
}

type Store struct {
	mu    sync.RWMutex
	dir   string
	posts []Post // sorted newest first
}

func OpenStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	b, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.posts); err != nil {
		return nil, fmt.Errorf("posts.json is corrupt: %w", err)
	}
	s.sort()
	return s, nil
}

func (s *Store) file() string { return filepath.Join(s.dir, "posts.json") }

func (s *Store) sort() {
	sort.SliceStable(s.posts, func(i, j int) bool {
		if s.posts[i].Date.Equal(s.posts[j].Date) {
			return s.posts[i].Created.After(s.posts[j].Created)
		}
		return s.posts[i].Date.After(s.posts[j].Date)
	})
}

// save writes posts.json atomically: a torn write during a crash would
// otherwise lose every post at once.
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.posts, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "posts-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.file())
}

func (s *Store) List() []Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Post, len(s.posts))
	copy(out, s.posts)
	return out
}

func (s *Store) Latest() (Post, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.posts) == 0 {
		return Post{}, false
	}
	return s.posts[0], true
}

// Get returns the post plus its neighbours: newer is the post above it in the
// timeline, older the one below.
func (s *Store) Get(id string) (p Post, newer, older *Post, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, x := range s.posts {
		if x.ID != id {
			continue
		}
		if i > 0 {
			n := s.posts[i-1]
			newer = &n
		}
		if i < len(s.posts)-1 {
			o := s.posts[i+1]
			older = &o
		}
		return x, newer, older, true
	}
	return Post{}, nil, nil, false
}

func (s *Store) Add(p Post) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = s.uniqueSlug(p.Date, p.Caption)
	s.posts = append(s.posts, p)
	s.sort()
	return s.save()
}

// Update changes a post's caption and date in place. The ID, image, and
// original file are deliberately left alone: keeping the ID stable is the whole
// point of editing rather than delete-and-repost, since the ID is the permalink
// and the RSS guid. Changing the date re-sorts the timeline but not the ID, so
// an old post keeps its original slug even if you correct its date.
func (s *Store) Update(id, caption string, date time.Time) (Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.posts {
		if s.posts[i].ID != id {
			continue
		}
		s.posts[i].Caption = caption
		s.posts[i].Date = date
		updated := s.posts[i]
		s.sort()
		if err := s.save(); err != nil {
			return Post{}, err
		}
		return updated, nil
	}
	return Post{}, errors.New("no such post")
}

func (s *Store) Delete(id string) (Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.posts {
		if p.ID != id {
			continue
		}
		s.posts = append(s.posts[:i], s.posts[i+1:]...)
		if err := s.save(); err != nil {
			return Post{}, err
		}
		for _, f := range []string{
			filepath.Join(s.dir, "media", p.Image),
			filepath.Join(s.dir, "media", p.Thumb),
			filepath.Join(s.dir, "originals", p.Original),
		} {
			if f != "" {
				_ = os.Remove(f)
			}
		}
		return p, nil
	}
	return Post{}, errors.New("no such post")
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// uniqueSlug builds 2026-07-13-sat-on-the-clean-laundry, with a counter if
// that day already carries the same caption. Caller must hold the lock.
func (s *Store) uniqueSlug(date time.Time, caption string) string {
	words := slugStrip.Split(strings.ToLower(caption), -1)
	kept := make([]string, 0, 6)
	for _, w := range words {
		if w == "" {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 6 {
			break
		}
	}
	base := date.Format("2006-01-02")
	if len(kept) > 0 {
		base += "-" + strings.Join(kept, "-")
	}
	if len(base) > 80 {
		base = strings.TrimRight(base[:80], "-")
	}

	taken := make(map[string]bool, len(s.posts))
	for _, p := range s.posts {
		taken[p.ID] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", base, i)
		if !taken[c] {
			return c
		}
	}
}
