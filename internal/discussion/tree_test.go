package discussion

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// thread builds comments from "id:parent" specs, oldest first; an empty parent
// means top-level.
func thread(spec ...string) []Comment {
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	out := make([]Comment, 0, len(spec))
	for i, s := range spec {
		id, parent, _ := strings.Cut(s, ":")
		c := Comment{
			ID:        id,
			PostID:    "p1",
			AuthorID:  "author-" + id,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if parent != "" {
			c.ParentID = &parent
		}
		out = append(out, c)
	}
	return out
}

func levels(comments []Comment) map[string]int {
	got := map[string]int{}
	Walk(BuildTree(comments, nil), func(n *CommentNode, level int) {
		got[n.ID] = level
	})
	return got
}

func TestNestingLevels(t *testing.T) {
	tests := []struct {
		name     string
		comments []Comment
		want     map[string]int
	}{
		{"no comments", nil, map[string]int{}},
		{"all top level", thread("a", "b", "c"), map[string]int{"a": 1, "b": 1, "c": 1}},
		{"one chain", thread("a", "b:a", "c:b", "d:c"), map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}},
		{
			"branches counted separately",
			thread("a", "b:a", "c:a", "d:c", "e", "f:d"),
			map[string]int{"a": 1, "b": 2, "c": 2, "d": 3, "e": 1, "f": 4},
		},
		{
			"reply listed before its parent",
			[]Comment{thread("b:a")[0], thread("a")[0]},
			map[string]int{"a": 1, "b": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := levels(tt.comments); !maps.Equal(got, tt.want) {
				t.Errorf("levels = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeepThreadStaysOffTheStack(t *testing.T) {
	const n = 100_000
	comments := thread("c0")
	for i := 1; i < n; i++ {
		comments = append(comments, thread(fmt.Sprintf("c%d:c%d", i, i-1))...)
	}

	e := ComputeEngagement(Post{ID: "p1"}, BuildTree(comments, nil), nil)
	if e.LongestReplyChain != n {
		t.Fatalf("LongestReplyChain = %d, want %d", e.LongestReplyChain, n)
	}
}

func TestBuildTree(t *testing.T) {
	comments := thread("a", "b:a", "c", "d:a", "e:b")
	likes := map[Target]int{
		{Type: TargetComment, ID: "a"}: 3,
		{Type: TargetComment, ID: "e"}: 1,
	}

	roots := BuildTree(comments, likes)

	if len(roots) != 2 || roots[0].ID != "a" || roots[1].ID != "c" {
		t.Fatalf("roots = %v, want [a c]", nodeIDs(roots))
	}
	if roots[0].Likes != 3 {
		t.Errorf("likes on a = %d, want 3", roots[0].Likes)
	}
	if got := nodeIDs(roots[0].Replies); got[0] != "b" || got[1] != "d" {
		t.Errorf("replies to a = %v, want [b d]", got)
	}
	if roots[0].Replies[0].Replies[0].ID != "e" {
		t.Error("e is not nested under b")
	}
	if roots[1].Replies == nil {
		t.Error("leaf replies = nil, want an empty slice")
	}
}

func TestComputeEngagement(t *testing.T) {
	post := Post{ID: "p1", AuthorID: "author"}
	comments := thread("a", "b:a", "c:b", "d") // authors are author-a .. author-d

	tests := []struct {
		name  string
		likes []Like
		want  Engagement
	}{
		{
			name: "comments only",
			want: Engagement{
				PostID: "p1", CommentCount: 4,
				DistinctEngagedUsers: 4,
				CommentsByDepth:      map[int]int{1: 2, 2: 1, 3: 1},
				LongestReplyChain:    3,
			},
		},
		{
			name: "likes on the post and on comments",
			likes: []Like{
				{Target: Target{TargetPost, "p1"}, UserID: "u1"},
				{Target: Target{TargetPost, "p1"}, UserID: "u2"},
				{Target: Target{TargetComment, "a"}, UserID: "u1"},
				{Target: Target{TargetComment, "c"}, UserID: "u3"},
			},
			want: Engagement{
				PostID: "p1", PostLikes: 2, CommentLikes: 2, CommentCount: 4,
				// four comment authors plus u1, u2, u3; u1 liked twice.
				DistinctEngagedUsers: 7,
				CommentsByDepth:      map[int]int{1: 2, 2: 1, 3: 1},
				LongestReplyChain:    3,
			},
		},
		{
			name:  "someone who commented and liked counts once",
			likes: []Like{{Target: Target{TargetPost, "p1"}, UserID: "author-a"}},
			want: Engagement{
				PostID: "p1", PostLikes: 1, CommentCount: 4,
				DistinctEngagedUsers: 4,
				CommentsByDepth:      map[int]int{1: 2, 2: 1, 3: 1},
				LongestReplyChain:    3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeEngagement(post, BuildTree(comments, nil), tt.likes)
			if !equalEngagement(got, tt.want) {
				t.Errorf("engagement =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestComputeEngagementOnAnUntouchedPost(t *testing.T) {
	got := ComputeEngagement(Post{ID: "p1", AuthorID: "author"}, nil, nil)
	want := Engagement{PostID: "p1", CommentsByDepth: map[int]int{}}
	if !equalEngagement(got, want) {
		t.Errorf("engagement = %+v, want %+v", got, want)
	}
}

func equalEngagement(a, b Engagement) bool {
	return a.PostID == b.PostID &&
		a.PostLikes == b.PostLikes &&
		a.CommentLikes == b.CommentLikes &&
		a.CommentCount == b.CommentCount &&
		a.DistinctEngagedUsers == b.DistinctEngagedUsers &&
		a.LongestReplyChain == b.LongestReplyChain &&
		maps.Equal(a.CommentsByDepth, b.CommentsByDepth)
}

func nodeIDs(nodes []*CommentNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func TestSelectPage(t *testing.T) {
	comments := thread("a", "a1:a", "b", "b1:b", "c", "c1:c", "d", "d1:d", "e", "e1:e")

	t.Run("first page stops at the limit", func(t *testing.T) {
		got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 2})

		if ids := nodeIDs(got); ids[0] != "a" || ids[1] != "b" || len(ids) != 2 {
			t.Fatalf("page = %v, want [a b]", ids)
		}
		if !info.HasMore || info.NextCursor != "b" {
			t.Errorf("info = %+v, want has_more with cursor b", info)
		}
		if info.TotalTopLevel != 5 {
			t.Errorf("TotalTopLevel = %d, want 5", info.TotalTopLevel)
		}
		if got[0].ReplyCount != 1 || got[0].Replies[0].ID != "a1" {
			t.Error("the first root lost its reply")
		}
	})

	t.Run("the cursor picks up where the last page stopped", func(t *testing.T) {
		got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 2, Cursor: "b"})
		if ids := nodeIDs(got); ids[0] != "c" || ids[1] != "d" {
			t.Fatalf("page = %v, want [c d]", ids)
		}
		if !info.HasMore {
			t.Error("has_more is false with one root still to come")
		}
	})

	t.Run("the last page says so", func(t *testing.T) {
		got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 2, Cursor: "d"})
		if ids := nodeIDs(got); len(ids) != 1 || ids[0] != "e" {
			t.Fatalf("page = %v, want [e]", ids)
		}
		if info.HasMore || info.NextCursor != "" {
			t.Errorf("info = %+v, want no more pages", info)
		}
	})

	t.Run("a cursor past the end gives an empty page", func(t *testing.T) {
		got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 2, Cursor: "zzz"})
		if len(got) != 0 {
			t.Fatalf("page = %v, want empty", nodeIDs(got))
		}
		if info.HasMore || info.NextCursor != "" {
			t.Errorf("info = %+v, want no more pages", info)
		}
	})

	t.Run("a limit past the end returns everything", func(t *testing.T) {
		got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 100})
		if len(got) != 5 || info.HasMore {
			t.Errorf("got %d roots with has_more %v, want 5 and false", len(got), info.HasMore)
		}
	})

	t.Run("paging through covers the thread exactly once", func(t *testing.T) {
		seen := []string{}
		cursor := ""
		for range 10 {
			got, info := SelectPage(BuildTree(comments, nil), Page{Limit: 2, Cursor: cursor})
			seen = append(seen, nodeIDs(got)...)
			if !info.HasMore {
				break
			}
			cursor = info.NextCursor
		}
		if want := []string{"a", "b", "c", "d", "e"}; !slices.Equal(seen, want) {
			t.Errorf("paged through %v, want %v", seen, want)
		}
	})
}

func TestSelectPageMaxDepth(t *testing.T) {
	comments := thread("a", "b:a", "c:b", "d:c")

	t.Run("depth zero keeps the whole chain", func(t *testing.T) {
		got, _ := SelectPage(BuildTree(comments, nil), Page{Limit: 10})
		if depth := chainLength(got[0]); depth != 4 {
			t.Errorf("chain length = %d, want 4", depth)
		}
	})

	t.Run("a depth limit cuts the chain and says what is below", func(t *testing.T) {
		got, _ := SelectPage(BuildTree(comments, nil), Page{Limit: 10, MaxDepth: 2})

		if depth := chainLength(got[0]); depth != 2 {
			t.Errorf("chain length = %d, want 2", depth)
		}
		cut := got[0].Replies[0]
		if cut.ReplyCount != 1 || len(cut.Replies) != 0 {
			t.Errorf("cut node has %d replies and reports %d, want 0 and 1",
				len(cut.Replies), cut.ReplyCount)
		}
	})
}

func chainLength(n *CommentNode) int {
	depth := 0
	for n != nil {
		depth++
		if len(n.Replies) == 0 {
			return depth
		}
		n = n.Replies[0]
	}
	return depth
}
