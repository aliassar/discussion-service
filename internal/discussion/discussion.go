// Package discussion is the domain: model, rules, and the Store it depends on.
// It knows nothing about HTTP or storage engines.
package discussion

import (
	"errors"
	"fmt"
	"time"
)

type TargetType string

const (
	TargetPost    TargetType = "post"
	TargetComment TargetType = "comment"
)

type Target struct {
	Type TargetType
	ID   string
}

type Post struct {
	ID        string    `json:"id"`
	AuthorID  string    `json:"author_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Comment is stored flat; ParentID links a reply to its parent and the tree
// is derived on read.
type Comment struct {
	ID        string    `json:"id"`
	PostID    string    `json:"post_id"`
	ParentID  *string   `json:"parent_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Like is unique per (Target, UserID).
type Like struct {
	Target    Target
	UserID    string
	CreatedAt time.Time
}

type CommentNode struct {
	Comment
	Likes int `json:"likes"`
	// ReplyCount can exceed len(Replies) when MaxDepth trims the tree.
	ReplyCount int            `json:"reply_count"`
	Replies    []*CommentNode `json:"replies"`
}

type PostView struct {
	Post
	Likes    int            `json:"likes"`
	Comments []*CommentNode `json:"comments"`
	Page     PageInfo       `json:"comments_page"`
}

// Page selects a window of a post's top-level comments.
type Page struct {
	Limit    int
	Cursor   string // last id of the previous page
	MaxDepth int    // 0 means unbounded
}

type PageInfo struct {
	Limit         int    `json:"limit"`
	NextCursor    string `json:"next_cursor,omitempty"`
	HasMore       bool   `json:"has_more"`
	TotalTopLevel int    `json:"total_top_level_comments"`
}

type Engagement struct {
	PostID       string `json:"post_id"`
	PostLikes    int    `json:"post_likes"`
	CommentLikes int    `json:"comment_likes"`
	CommentCount int    `json:"comment_count"`
	// Users who commented or liked anything in the thread; authoring the post
	// alone does not count.
	DistinctEngagedUsers int `json:"distinct_engaged_users"`
	// Comments per nesting level; a direct comment on the post is level 1.
	CommentsByDepth map[int]int `json:"comments_by_depth"`
	// Comments on the longest root-to-leaf path.
	LongestReplyChain int `json:"longest_reply_chain"`
}

var ErrNotFound = errors.New("not found")

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// NotFoundError is how every Store reports a missing record.
func NotFoundError(kind, id string) error {
	return fmt.Errorf("%s %q: %w", kind, id, ErrNotFound)
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}
