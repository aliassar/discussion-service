// Package memory is an in-process discussion.Store.
package memory

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/aliassar/discussion-service/internal/discussion"
)

// Store guards all state with one RWMutex. Every operation is a few map
// lookups, so finer-grained locking would not pay for itself.
type Store struct {
	mu sync.RWMutex

	posts      map[string]discussion.Post
	comments   map[string]discussion.Comment
	commentIDs map[string][]string // by post
	// Keyed by target, then user: one like per user per item is structural.
	likes map[discussion.Target]map[string]discussion.Like
	keys  map[string]string // idempotency key -> record id
}

func New() *Store {
	return &Store{
		posts:      map[string]discussion.Post{},
		comments:   map[string]discussion.Comment{},
		commentIDs: map[string][]string{},
		likes:      map[discussion.Target]map[string]discussion.Like{},
		keys:       map[string]string{},
	}
}

var _ discussion.Store = (*Store)(nil)

func (s *Store) CreatePost(_ context.Context, p discussion.Post) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts[p.ID] = p
	return nil
}

func (s *Store) Post(_ context.Context, id string) (discussion.Post, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.posts[id]
	if !ok {
		return discussion.Post{}, discussion.NotFoundError("post", id)
	}
	return p, nil
}

func (s *Store) CreateComment(_ context.Context, c discussion.Comment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments[c.ID] = c
	s.commentIDs[c.PostID] = append(s.commentIDs[c.PostID], c.ID)
	return nil
}

func (s *Store) Comment(_ context.Context, id string) (discussion.Comment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.comments[id]
	if !ok {
		return discussion.Comment{}, discussion.NotFoundError("comment", id)
	}
	return c, nil
}

func (s *Store) CommentsByPost(_ context.Context, postID string) ([]discussion.Comment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]discussion.Comment, 0, len(s.commentIDs[postID]))
	for _, id := range s.commentIDs[postID] {
		out = append(out, s.comments[id])
	}
	slices.SortFunc(out, func(a, b discussion.Comment) int {
		if d := a.CreatedAt.Compare(b.CreatedAt); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (s *Store) AddLike(_ context.Context, l discussion.Like) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.likes[l.Target] == nil {
		s.likes[l.Target] = map[string]discussion.Like{}
	}
	if _, exists := s.likes[l.Target][l.UserID]; !exists {
		s.likes[l.Target][l.UserID] = l
	}
	return nil
}

func (s *Store) RemoveLike(_ context.Context, t discussion.Target, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.likes[t], userID)
	if len(s.likes[t]) == 0 {
		delete(s.likes, t)
	}
	return nil
}

func (s *Store) LikesForTargets(_ context.Context, targets []discussion.Target) ([]discussion.Like, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []discussion.Like
	for _, t := range targets {
		for _, l := range s.likes[t] {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *Store) ClaimKey(_ context.Context, key, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if owner, ok := s.keys[key]; ok {
		return owner, nil
	}
	s.keys[key] = id
	return id, nil
}
