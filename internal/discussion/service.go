package discussion

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Free-text limits, counted in characters rather than bytes.
const (
	maxTitleLen = 200
	maxBodyLen  = 10_000
	maxIDLen    = 64
)

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
	maxPageDepth     = 100
)

// Store persists whole records; trees and metrics are derived by the Service,
// so an implementation needs only plain reads and writes.
type Store interface {
	CreatePost(ctx context.Context, p Post) error
	Post(ctx context.Context, id string) (Post, error)

	CreateComment(ctx context.Context, c Comment) error
	Comment(ctx context.Context, id string) (Comment, error)
	// CommentsByPost returns comments oldest first.
	CommentsByPost(ctx context.Context, postID string) ([]Comment, error)

	// AddLike and RemoveLike are idempotent.
	AddLike(ctx context.Context, l Like) error
	RemoveLike(ctx context.Context, t Target, userID string) error
	LikesForTargets(ctx context.Context, targets []Target) ([]Like, error)

	// ClaimKey binds an idempotency key to id on first use and returns the id
	// it is bound to. The check and the write must be one atomic step.
	ClaimKey(ctx context.Context, key, id string) (string, error)
}

type Service struct {
	store Store
	ids   idFactory
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

type CreatePostInput struct {
	AuthorID       string
	Title          string
	Body           string
	IdempotencyKey string
}

// CreatePost returns created=false when it replays an earlier request with
// the same idempotency key.
func (s *Service) CreatePost(ctx context.Context, in CreatePostInput) (post Post, created bool, err error) {
	author, err := text("author_id", in.AuthorID, maxIDLen)
	if err != nil {
		return Post{}, false, err
	}
	title, err := text("title", in.Title, maxTitleLen)
	if err != nil {
		return Post{}, false, err
	}
	body, err := text("body", in.Body, maxBodyLen)
	if err != nil {
		return Post{}, false, err
	}

	id, at := s.ids.next()
	first, replay, err := s.claim(ctx, "post", in.IdempotencyKey, id)
	if err != nil {
		return Post{}, false, err
	}
	if replay {
		p, err := s.store.Post(ctx, first)
		return p, false, err
	}

	p := Post{ID: id, AuthorID: author, Title: title, Body: body, CreatedAt: at}
	if err := s.store.CreatePost(ctx, p); err != nil {
		return Post{}, false, err
	}
	return p, true, nil
}

type CreateCommentInput struct {
	PostID         string
	ParentID       *string
	AuthorID       string
	Body           string
	IdempotencyKey string
}

func (s *Service) CreateComment(ctx context.Context, in CreateCommentInput) (comment Comment, created bool, err error) {
	author, err := text("author_id", in.AuthorID, maxIDLen)
	if err != nil {
		return Comment{}, false, err
	}
	body, err := text("body", in.Body, maxBodyLen)
	if err != nil {
		return Comment{}, false, err
	}
	if _, err := s.store.Post(ctx, in.PostID); err != nil {
		return Comment{}, false, err
	}

	var parentID *string
	if in.ParentID != nil {
		parent, err := s.store.Comment(ctx, *in.ParentID)
		if err != nil {
			return Comment{}, false, err
		}
		if parent.PostID != in.PostID {
			return Comment{}, false, invalid("parent_id", "belongs to a different post")
		}
		parentID = &parent.ID
	}

	id, at := s.ids.next()
	first, replay, err := s.claim(ctx, "comment", in.IdempotencyKey, id)
	if err != nil {
		return Comment{}, false, err
	}
	if replay {
		c, err := s.store.Comment(ctx, first)
		return c, false, err
	}

	c := Comment{
		ID: id, PostID: in.PostID, ParentID: parentID,
		AuthorID: author, Body: body, CreatedAt: at,
	}
	if err := s.store.CreateComment(ctx, c); err != nil {
		return Comment{}, false, err
	}
	return c, true, nil
}

func (s *Service) Like(ctx context.Context, t Target, userID string) error {
	user, err := text("user_id", userID, maxIDLen)
	if err != nil {
		return err
	}
	if err := s.requireTarget(ctx, t); err != nil {
		return err
	}
	return s.store.AddLike(ctx, Like{Target: t, UserID: user, CreatedAt: now()})
}

// Unlike still requires the target to exist, so a mistyped id is a 404
// rather than a silent no-op.
func (s *Service) Unlike(ctx context.Context, t Target, userID string) error {
	user, err := text("user_id", userID, maxIDLen)
	if err != nil {
		return err
	}
	if err := s.requireTarget(ctx, t); err != nil {
		return err
	}
	return s.store.RemoveLike(ctx, t, user)
}

func (s *Service) PostView(ctx context.Context, postID string, page Page) (PostView, error) {
	page, err := page.check()
	if err != nil {
		return PostView{}, err
	}
	post, tree, likes, err := s.thread(ctx, postID)
	if err != nil {
		return PostView{}, err
	}

	comments, info := SelectPage(tree, page)
	view := PostView{Post: post, Comments: comments, Page: info}
	for _, l := range likes {
		if l.Target.Type == TargetPost {
			view.Likes++
		}
	}
	return view, nil
}

func (s *Service) Engagement(ctx context.Context, postID string) (Engagement, error) {
	post, tree, likes, err := s.thread(ctx, postID)
	if err != nil {
		return Engagement{}, err
	}
	return ComputeEngagement(post, tree, likes), nil
}

// thread is three reads, not one snapshot: a comment written between the last
// two is counted without its likes. A SQL store would use one read-only
// transaction.
func (s *Service) thread(ctx context.Context, postID string) (Post, []*CommentNode, []Like, error) {
	post, err := s.store.Post(ctx, postID)
	if err != nil {
		return Post{}, nil, nil, err
	}
	comments, err := s.store.CommentsByPost(ctx, postID)
	if err != nil {
		return Post{}, nil, nil, err
	}

	targets := make([]Target, 0, len(comments)+1)
	targets = append(targets, Target{Type: TargetPost, ID: post.ID})
	for _, c := range comments {
		targets = append(targets, Target{Type: TargetComment, ID: c.ID})
	}
	likes, err := s.store.LikesForTargets(ctx, targets)
	if err != nil {
		return Post{}, nil, nil, err
	}

	counts := make(map[Target]int, len(targets))
	for _, l := range likes {
		counts[l.Target]++
	}
	return post, BuildTree(comments, counts), likes, nil
}

func (s *Service) requireTarget(ctx context.Context, t Target) error {
	switch t.Type {
	case TargetPost:
		_, err := s.store.Post(ctx, t.ID)
		return err
	case TargetComment:
		_, err := s.store.Comment(ctx, t.ID)
		return err
	default:
		return invalid("target", "unknown target type %q", t.Type)
	}
}

// claim scopes keys by kind, so one key can serve a post and a comment.
func (s *Service) claim(ctx context.Context, kind, key, id string) (first string, replay bool, err error) {
	if key == "" {
		return "", false, nil
	}
	owner, err := s.store.ClaimKey(ctx, kind+":"+key, id)
	if err != nil {
		return "", false, err
	}
	return owner, owner != id, nil
}

func text(field, value string, maxLen int) (string, error) {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		return "", invalid(field, "is required")
	case !utf8.ValidString(v):
		return "", invalid(field, "must be valid UTF-8")
	case utf8.RuneCountInString(v) > maxLen:
		return "", invalid(field, "must be at most %d characters", maxLen)
	}
	return v, nil
}

func now() time.Time { return time.Now().UTC() }

// idFactory issues ids that sort in creation order: timestamp, counter,
// random. The clock is only microsecond-grained, so the counter is what
// orders records created within one tick; comment order and cursor paging
// depend on it. Ordering is exact within a process, not across processes.
type idFactory struct {
	mu   sync.Mutex
	last int64
	seq  uint16
}

// next derives the id and created_at from one clock reading, so sorting by
// id equals sorting by (created_at, id).
func (f *idFactory) next() (id string, at time.Time) {
	nano := time.Now().UnixNano()

	f.mu.Lock()
	if nano > f.last {
		f.last, f.seq = nano, 0
	} else {
		// Same tick, or the clock stepped back: stay on the last tick.
		f.seq++
		if f.seq == 0 {
			f.last++ // counter wrapped
		}
		nano = f.last
	}
	seq := f.seq
	f.mu.Unlock()

	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(nano))
	binary.BigEndian.PutUint16(b[8:10], seq)
	_, _ = rand.Read(b[10:])
	return hex.EncodeToString(b[:]), time.Unix(0, nano).UTC()
}

func (p Page) check() (Page, error) {
	if p.Limit == 0 {
		p.Limit = defaultPageLimit
	}
	switch {
	case p.Limit < 1 || p.Limit > maxPageLimit:
		return p, invalid("limit", "must be between 1 and %d", maxPageLimit)
	case p.MaxDepth < 0 || p.MaxDepth > maxPageDepth:
		return p, invalid("max_depth", "must be between 0 and %d", maxPageDepth)
	case len(p.Cursor) > maxIDLen:
		return p, invalid("cursor", "is not a cursor this API issued")
	}
	return p, nil
}
