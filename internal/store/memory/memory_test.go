package memory_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aliassar/discussion-service/internal/discussion"
	"github.com/aliassar/discussion-service/internal/store/memory"
)

// Service tests cover the store end to end; these cover what only the store
// can show.

func TestMissingRecords(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	if _, err := s.Post(ctx, "nope"); !errors.Is(err, discussion.ErrNotFound) {
		t.Errorf("Post = %v, want ErrNotFound", err)
	}
	if _, err := s.Comment(ctx, "nope"); !errors.Is(err, discussion.ErrNotFound) {
		t.Errorf("Comment = %v, want ErrNotFound", err)
	}
}

func TestCommentsByPostIsScopedAndOrdered(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	// Out of order, one on another post, and two sharing a timestamp.
	for _, c := range []discussion.Comment{
		{ID: "c3", PostID: "p1", CreatedAt: at.Add(2 * time.Second)},
		{ID: "c1", PostID: "p1", CreatedAt: at},
		{ID: "elsewhere", PostID: "p2", CreatedAt: at},
		{ID: "c2", PostID: "p1", CreatedAt: at},
	} {
		if err := s.CreateComment(ctx, c); err != nil {
			t.Fatalf("CreateComment: %v", err)
		}
	}

	got, err := s.CommentsByPost(ctx, "p1")
	if err != nil {
		t.Fatalf("CommentsByPost: %v", err)
	}

	want := []string{"c1", "c2", "c3"}
	if len(got) != len(want) {
		t.Fatalf("got %d comments, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("comment %d = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestLikesAreKeyedByTypeAsWellAsID(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	onComment := discussion.Target{Type: discussion.TargetComment, ID: "x1"}
	onPost := discussion.Target{Type: discussion.TargetPost, ID: "x1"}

	for _, target := range []discussion.Target{onComment, onPost} {
		if err := s.AddLike(ctx, discussion.Like{Target: target, UserID: "bob"}); err != nil {
			t.Fatalf("AddLike: %v", err)
		}
	}

	likes, err := s.LikesForTargets(ctx, []discussion.Target{onComment})
	if err != nil {
		t.Fatalf("LikesForTargets: %v", err)
	}
	if len(likes) != 1 || likes[0].Target != onComment {
		t.Errorf("likes = %+v, want only the like on %+v", likes, onComment)
	}
}

func TestRemoveLikeThenLikeAgain(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	target := discussion.Target{Type: discussion.TargetComment, ID: "c1"}

	if err := s.RemoveLike(ctx, target, "bob"); err != nil {
		t.Fatalf("RemoveLike on an empty store: %v", err)
	}
	if err := s.AddLike(ctx, discussion.Like{Target: target, UserID: "bob"}); err != nil {
		t.Fatalf("AddLike: %v", err)
	}
	if err := s.RemoveLike(ctx, target, "bob"); err != nil {
		t.Fatalf("RemoveLike: %v", err)
	}
	if err := s.AddLike(ctx, discussion.Like{Target: target, UserID: "bob"}); err != nil {
		t.Fatalf("AddLike after removing: %v", err)
	}

	likes, err := s.LikesForTargets(ctx, []discussion.Target{target})
	if err != nil {
		t.Fatalf("LikesForTargets: %v", err)
	}
	if len(likes) != 1 {
		t.Errorf("got %d likes, want 1", len(likes))
	}
}

func TestClaimKeyPicksOneWinner(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners = map[string]int{}
	)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner, err := s.ClaimKey(ctx, "key", fmt.Sprintf("id-%d", i))
			if err != nil {
				t.Errorf("ClaimKey: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			winners[owner]++
		}()
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Errorf("the key resolved to %d different ids, want 1", len(winners))
	}
}
