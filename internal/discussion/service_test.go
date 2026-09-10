package discussion_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aliassar/discussion-service/internal/discussion"
	"github.com/aliassar/discussion-service/internal/store/memory"
)

func newService() *discussion.Service {
	return discussion.NewService(memory.New())
}

func newPost(t *testing.T, svc *discussion.Service, author string) discussion.Post {
	t.Helper()
	p, created, err := svc.CreatePost(context.Background(), discussion.CreatePostInput{
		AuthorID: author, Title: "title", Body: "body",
	})
	if err != nil || !created {
		t.Fatalf("CreatePost = (created %v, %v), want (true, nil)", created, err)
	}
	return p
}

func newComment(t *testing.T, svc *discussion.Service, postID string, parentID *string, author string) discussion.Comment {
	t.Helper()
	c, created, err := svc.CreateComment(context.Background(), discussion.CreateCommentInput{
		PostID: postID, ParentID: parentID, AuthorID: author, Body: "a comment",
	})
	if err != nil || !created {
		t.Fatalf("CreateComment = (created %v, %v), want (true, nil)", created, err)
	}
	return c
}

func TestCreatePostThenReadItBack(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")

	if post.ID == "" || post.CreatedAt.IsZero() {
		t.Fatalf("post is missing an id or a timestamp: %+v", post)
	}

	view, err := svc.PostView(context.Background(), post.ID, discussion.Page{})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}
	if view.ID != post.ID || view.AuthorID != "alice" {
		t.Errorf("view = %+v, want the post we just created", view.Post)
	}
	if len(view.Comments) != 0 || view.Likes != 0 {
		t.Errorf("a new post has %d comments and %d likes, want 0 and 0", len(view.Comments), view.Likes)
	}
}

func TestCreatePostRejectsBadInput(t *testing.T) {
	tests := []struct {
		name  string
		in    discussion.CreatePostInput
		field string
	}{
		{"no author", discussion.CreatePostInput{Title: "t", Body: "b"}, "author_id"},
		{"blank author", discussion.CreatePostInput{AuthorID: "   ", Title: "t", Body: "b"}, "author_id"},
		{"no title", discussion.CreatePostInput{AuthorID: "a", Body: "b"}, "title"},
		{"no body", discussion.CreatePostInput{AuthorID: "a", Title: "t"}, "body"},
		{
			"title too long",
			discussion.CreatePostInput{AuthorID: "a", Title: strings.Repeat("x", 201), Body: "b"},
			"title",
		},
		{
			"invalid UTF-8 in body",
			discussion.CreatePostInput{AuthorID: "a", Title: "t", Body: "bad \xc3\x28"},
			"body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := newService().CreatePost(context.Background(), tt.in)

			var invalid *discussion.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
			if invalid.Field != tt.field {
				t.Errorf("field = %q, want %q", invalid.Field, tt.field)
			}
		})
	}
}

func TestLimitsCountCharactersNotBytes(t *testing.T) {
	svc := newService()
	_, _, err := svc.CreatePost(context.Background(), discussion.CreatePostInput{
		AuthorID: "alice",
		Title:    strings.Repeat("é", 200), // 400 bytes
		Body:     "body",
	})
	if err != nil {
		t.Fatalf("200-character title rejected: %v", err)
	}

	_, _, err = svc.CreatePost(context.Background(), discussion.CreatePostInput{
		AuthorID: "alice",
		Title:    strings.Repeat("é", 201),
		Body:     "body",
	})
	if err == nil {
		t.Fatal("201-character title accepted, want it rejected")
	}
}

func TestCreateCommentBuildsATree(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")

	top := newComment(t, svc, post.ID, nil, "bob")
	reply := newComment(t, svc, post.ID, &top.ID, "carol")
	newComment(t, svc, post.ID, &reply.ID, "bob")
	second := newComment(t, svc, post.ID, nil, "dave")

	view, err := svc.PostView(context.Background(), post.ID, discussion.Page{})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}

	if len(view.Comments) != 2 {
		t.Fatalf("top-level comments = %d, want 2", len(view.Comments))
	}
	if view.Comments[0].ID != top.ID || view.Comments[1].ID != second.ID {
		t.Error("top-level comments are not in creation order")
	}
	if got := view.Comments[0].Replies[0].Replies[0].AuthorID; got != "bob" {
		t.Errorf("author at level 3 = %q, want bob", got)
	}
}

func TestCreateCommentRejectsBadInput(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	post := newPost(t, svc, "alice")
	elsewhere := newPost(t, svc, "alice")
	otherThread := newComment(t, svc, elsewhere.ID, nil, "bob")
	missing := "does-not-exist"

	t.Run("post does not exist", func(t *testing.T) {
		_, _, err := svc.CreateComment(ctx, discussion.CreateCommentInput{
			PostID: missing, AuthorID: "bob", Body: "hi",
		})
		requireNotFound(t, err)
	})

	t.Run("parent does not exist", func(t *testing.T) {
		_, _, err := svc.CreateComment(ctx, discussion.CreateCommentInput{
			PostID: post.ID, ParentID: &missing, AuthorID: "bob", Body: "hi",
		})
		requireNotFound(t, err)
	})

	t.Run("parent is on another post", func(t *testing.T) {
		_, _, err := svc.CreateComment(ctx, discussion.CreateCommentInput{
			PostID: post.ID, ParentID: &otherThread.ID, AuthorID: "bob", Body: "hi",
		})
		requireInvalid(t, err, "parent_id")
	})

	t.Run("blank body", func(t *testing.T) {
		_, _, err := svc.CreateComment(ctx, discussion.CreateCommentInput{
			PostID: post.ID, AuthorID: "bob", Body: "   ",
		})
		requireInvalid(t, err, "body")
	})
}

func TestIdempotentCreate(t *testing.T) {
	svc := newService()
	ctx := context.Background()

	t.Run("the same key returns the first post", func(t *testing.T) {
		in := discussion.CreatePostInput{
			AuthorID: "alice", Title: "t", Body: "b", IdempotencyKey: "key-1",
		}
		first, created, err := svc.CreatePost(ctx, in)
		if err != nil || !created {
			t.Fatalf("first call = (created %v, %v), want (true, nil)", created, err)
		}

		again, created, err := svc.CreatePost(ctx, in)
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if created {
			t.Error("second call reported a new post, want a replay")
		}
		if again.ID != first.ID {
			t.Errorf("replay id = %q, want %q", again.ID, first.ID)
		}
	})

	t.Run("a different key makes a second post", func(t *testing.T) {
		a, _, err := svc.CreatePost(ctx, discussion.CreatePostInput{
			AuthorID: "alice", Title: "t", Body: "b", IdempotencyKey: "key-2",
		})
		if err != nil {
			t.Fatalf("CreatePost: %v", err)
		}
		b, _, err := svc.CreatePost(ctx, discussion.CreatePostInput{
			AuthorID: "alice", Title: "t", Body: "b", IdempotencyKey: "key-3",
		})
		if err != nil {
			t.Fatalf("CreatePost: %v", err)
		}
		if a.ID == b.ID {
			t.Error("different keys produced the same post")
		}
	})

	t.Run("keys do not collide across posts and comments", func(t *testing.T) {
		post := newPost(t, svc, "alice")
		comment, created, err := svc.CreateComment(ctx, discussion.CreateCommentInput{
			PostID: post.ID, AuthorID: "bob", Body: "hi", IdempotencyKey: "key-1",
		})
		if err != nil || !created {
			t.Fatalf("CreateComment = (created %v, %v), want (true, nil)", created, err)
		}
		if comment.ID == "" {
			t.Error("comment has no id")
		}
	})

	t.Run("validation runs before a key is spent", func(t *testing.T) {
		bad := discussion.CreatePostInput{AuthorID: "", Title: "t", Body: "b", IdempotencyKey: "key-4"}
		if _, _, err := svc.CreatePost(ctx, bad); err == nil {
			t.Fatal("invalid input accepted")
		}
		good := discussion.CreatePostInput{AuthorID: "alice", Title: "t", Body: "b", IdempotencyKey: "key-4"}
		if _, created, err := svc.CreatePost(ctx, good); err != nil || !created {
			t.Fatalf("retry after a rejected request = (created %v, %v), want (true, nil)", created, err)
		}
	})
}

func TestConcurrentCreatesWithOneKeyMakeOnePost(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	in := discussion.CreatePostInput{
		AuthorID: "alice", Title: "t", Body: "b", IdempotencyKey: "shared",
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]bool{}
		creates int
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post, created, err := svc.CreatePost(ctx, in)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("CreatePost: %v", err)
				return
			}
			ids[post.ID] = true
			if created {
				creates++
			}
		}()
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("got %d distinct posts, want 1", len(ids))
	}
	if creates != 1 {
		t.Errorf("%d callers were told they created the post, want 1", creates)
	}
}

func TestLikeIsIdempotent(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	post := newPost(t, svc, "alice")
	target := discussion.Target{Type: discussion.TargetPost, ID: post.ID}

	for range 3 {
		if err := svc.Like(ctx, target, "bob"); err != nil {
			t.Fatalf("Like: %v", err)
		}
	}
	if err := svc.Like(ctx, target, "carol"); err != nil {
		t.Fatalf("Like: %v", err)
	}

	view, err := svc.PostView(ctx, post.ID, discussion.Page{})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}
	if view.Likes != 2 {
		t.Errorf("likes = %d, want 2", view.Likes)
	}
}

func TestUnlike(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	post := newPost(t, svc, "alice")
	comment := newComment(t, svc, post.ID, nil, "bob")
	target := discussion.Target{Type: discussion.TargetComment, ID: comment.ID}

	if err := svc.Like(ctx, target, "carol"); err != nil {
		t.Fatalf("Like: %v", err)
	}
	for _, user := range []string{"carol", "carol", "nobody"} {
		if err := svc.Unlike(ctx, target, user); err != nil {
			t.Fatalf("Unlike by %s: %v", user, err)
		}
	}

	view, err := svc.PostView(ctx, post.ID, discussion.Page{})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}
	if view.Comments[0].Likes != 0 {
		t.Errorf("likes = %d, want 0", view.Comments[0].Likes)
	}
}

func TestLikeNeedsATargetThatExists(t *testing.T) {
	svc := newService()
	ctx := context.Background()

	for _, target := range []discussion.Target{
		{Type: discussion.TargetPost, ID: "nope"},
		{Type: discussion.TargetComment, ID: "nope"},
	} {
		requireNotFound(t, svc.Like(ctx, target, "bob"))
		requireNotFound(t, svc.Unlike(ctx, target, "bob"))
	}
}

func TestLikeNeedsAUser(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")
	target := discussion.Target{Type: discussion.TargetPost, ID: post.ID}

	requireInvalid(t, svc.Like(context.Background(), target, "  "), "user_id")
}

func TestEngagement(t *testing.T) {
	svc := newService()
	ctx := context.Background()

	// alice's post:
	//   c1 (bob)          level 1
	//     c2 (carol)      level 2
	//       c3 (bob)      level 3
	//     c4 (dave)       level 2
	//   c5 (erin)         level 1
	post := newPost(t, svc, "alice")
	c1 := newComment(t, svc, post.ID, nil, "bob")
	c2 := newComment(t, svc, post.ID, &c1.ID, "carol")
	c3 := newComment(t, svc, post.ID, &c2.ID, "bob")
	newComment(t, svc, post.ID, &c1.ID, "dave")
	newComment(t, svc, post.ID, nil, "erin")

	// frank only ever likes; bob and carol both commented and liked.
	like(t, svc, discussion.TargetPost, post.ID, "bob")
	like(t, svc, discussion.TargetPost, post.ID, "frank")
	like(t, svc, discussion.TargetComment, c1.ID, "carol")
	like(t, svc, discussion.TargetComment, c1.ID, "frank")
	like(t, svc, discussion.TargetComment, c3.ID, "bob")

	got, err := svc.Engagement(ctx, post.ID)
	if err != nil {
		t.Fatalf("Engagement: %v", err)
	}

	want := discussion.Engagement{
		PostID: post.ID, PostLikes: 2, CommentLikes: 3, CommentCount: 5,
		// bob, carol, dave, erin, frank. alice wrote the post and did
		// nothing else.
		DistinctEngagedUsers: 5,
		CommentsByDepth:      map[int]int{1: 2, 2: 2, 3: 1},
		LongestReplyChain:    3,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("engagement =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestReadsNeedAPostThatExists(t *testing.T) {
	svc := newService()
	ctx := context.Background()

	_, err := svc.PostView(ctx, "nope", discussion.Page{})
	requireNotFound(t, err)

	_, err = svc.Engagement(ctx, "nope")
	requireNotFound(t, err)
}

// Run with -race: this also exercises the store's locking.
func TestConcurrentLikes(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	post := newPost(t, svc, "alice")
	target := discussion.Target{Type: discussion.TargetPost, ID: post.ID}

	const users, tries = 20, 10
	var wg sync.WaitGroup
	for u := range users {
		for range tries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := svc.Like(ctx, target, fmt.Sprintf("user-%d", u)); err != nil {
					t.Errorf("Like: %v", err)
				}
			}()
		}
	}
	wg.Wait()

	view, err := svc.PostView(ctx, post.ID, discussion.Page{})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}
	if view.Likes != users {
		t.Errorf("likes = %d, want %d", view.Likes, users)
	}
}

func like(t *testing.T, svc *discussion.Service, kind discussion.TargetType, id, user string) {
	t.Helper()
	if err := svc.Like(context.Background(), discussion.Target{Type: kind, ID: id}, user); err != nil {
		t.Fatalf("Like(%s %s by %s): %v", kind, id, user, err)
	}
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, discussion.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func requireInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var invalid *discussion.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
	if invalid.Field != field {
		t.Errorf("field = %q, want %q", invalid.Field, field)
	}
}

func TestPostViewPaging(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	post := newPost(t, svc, "alice")

	// 25 comments, so the default limit of 20 leaves a second page.
	var want []string
	for i := range 25 {
		c := newComment(t, svc, post.ID, nil, fmt.Sprintf("user-%d", i))
		want = append(want, c.ID)
	}

	t.Run("the default limit applies when none is asked for", func(t *testing.T) {
		view, err := svc.PostView(ctx, post.ID, discussion.Page{})
		if err != nil {
			t.Fatalf("PostView: %v", err)
		}
		if len(view.Comments) != 20 || view.Page.Limit != 20 {
			t.Errorf("got %d comments at limit %d, want 20 and 20", len(view.Comments), view.Page.Limit)
		}
		if !view.Page.HasMore || view.Page.TotalTopLevel != 25 {
			t.Errorf("page = %+v, want has_more with 25 in total", view.Page)
		}
	})

	t.Run("paging through returns every comment once, in order", func(t *testing.T) {
		var got []string
		cursor := ""
		for range 10 {
			view, err := svc.PostView(ctx, post.ID, discussion.Page{Limit: 7, Cursor: cursor})
			if err != nil {
				t.Fatalf("PostView: %v", err)
			}
			for _, c := range view.Comments {
				got = append(got, c.ID)
			}
			if !view.Page.HasMore {
				break
			}
			cursor = view.Page.NextCursor
		}
		if !slices.Equal(got, want) {
			t.Errorf("paged through %d comments, want the %d created, in order", len(got), len(want))
		}
	})

	t.Run("engagement still covers the whole thread", func(t *testing.T) {
		e, err := svc.Engagement(ctx, post.ID)
		if err != nil {
			t.Fatalf("Engagement: %v", err)
		}
		if e.CommentCount != 25 {
			t.Errorf("CommentCount = %d, want 25", e.CommentCount)
		}
	})
}

func TestPostViewRejectsBadPaging(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")

	tests := []struct {
		name  string
		page  discussion.Page
		field string
	}{
		{"limit below one", discussion.Page{Limit: -1}, "limit"},
		{"limit over the ceiling", discussion.Page{Limit: 101}, "limit"},
		{"negative depth", discussion.Page{MaxDepth: -1}, "max_depth"},
		{"depth over the ceiling", discussion.Page{MaxDepth: 101}, "max_depth"},
		{"cursor far too long", discussion.Page{Cursor: strings.Repeat("x", 65)}, "cursor"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.PostView(context.Background(), post.ID, tt.page)
			requireInvalid(t, err, tt.field)
		})
	}
}

// The clock is coarser than the write rate, so these ids share timestamps and
// only the generator's counter keeps them in creation order.
func TestIDsSortInCreationOrder(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")

	ids := make([]string, 0, 200)
	for range 200 {
		ids = append(ids, newComment(t, svc, post.ID, nil, "bob").ID)
	}

	if !slices.IsSorted(ids) {
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("id %d (%s) does not sort after id %d (%s)", i, ids[i], i-1, ids[i-1])
			}
		}
	}
	view, err := svc.PostView(context.Background(), post.ID, discussion.Page{Limit: 100})
	if err != nil {
		t.Fatalf("PostView: %v", err)
	}
	for i, comment := range view.Comments {
		if comment.ID != ids[i] {
			t.Fatalf("comment %d read back as %s, want %s", i, comment.ID, ids[i])
		}
	}
}

func TestConcurrentIDsAreUnique(t *testing.T) {
	svc := newService()
	post := newPost(t, svc, "alice")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := svc.CreateComment(context.Background(), discussion.CreateCommentInput{
				PostID: post.ID, AuthorID: "bob", Body: "x",
			})
			if err != nil {
				t.Errorf("CreateComment: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[c.ID] {
				t.Errorf("duplicate id %s", c.ID)
			}
			seen[c.ID] = true
		}()
	}
	wg.Wait()

	if len(seen) != 100 {
		t.Errorf("got %d distinct ids, want 100", len(seen))
	}
}
