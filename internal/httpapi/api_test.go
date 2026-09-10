package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aliassar/discussion-service/internal/discussion"
	"github.com/aliassar/discussion-service/internal/httpapi"
	"github.com/aliassar/discussion-service/internal/store/memory"
)

const prefix = "/v1"

type client struct {
	t    *testing.T
	base string
}

// as binds the helpers to a subtest's T, so failures are reported there.
func (c *client) as(t *testing.T) *client {
	return &client{t: t, base: c.base}
}

func newClient(t *testing.T) *client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.NewRouter(discussion.NewService(memory.New()), log))
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL}
}

func (c *client) send(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.base+prefix+path, reader)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (c *client) expect(method, path string, body any, status int, out any) {
	c.t.Helper()

	resp, raw := c.send(method, path, body, nil)
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s: status %d, want %d (%s)", method, path, resp.StatusCode, status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decoding %s: %v", method, path, raw, err)
		}
	}
}

func (c *client) createPost(author string) discussion.Post {
	c.t.Helper()
	var post discussion.Post
	c.expect(http.MethodPost, "/posts", map[string]string{
		"author_id": author, "title": "a title", "body": "a body",
	}, http.StatusCreated, &post)
	return post
}

func (c *client) createComment(postID, author string, parentID *string) discussion.Comment {
	c.t.Helper()
	req := map[string]any{"author_id": author, "body": "a comment"}
	if parentID != nil {
		req["parent_id"] = *parentID
	}
	var comment discussion.Comment
	c.expect(http.MethodPost, "/posts/"+postID+"/comments", req, http.StatusCreated, &comment)
	return comment
}

func TestThreadLifecycle(t *testing.T) {
	c := newClient(t)
	post := c.createPost("alice")

	//   c1 (bob)
	//     c2 (carol)
	//       c3 (bob)
	//   c4 (dave)
	c1 := c.createComment(post.ID, "bob", nil)
	c2 := c.createComment(post.ID, "carol", &c1.ID)
	c3 := c.createComment(post.ID, "bob", &c2.ID)
	c.createComment(post.ID, "dave", nil)

	c.expect(http.MethodPut, "/posts/"+post.ID+"/likes/bob", nil, http.StatusNoContent, nil)
	c.expect(http.MethodPut, "/posts/"+post.ID+"/likes/erin", nil, http.StatusNoContent, nil)
	c.expect(http.MethodPut, "/comments/"+c1.ID+"/likes/carol", nil, http.StatusNoContent, nil)
	c.expect(http.MethodPut, "/comments/"+c3.ID+"/likes/carol", nil, http.StatusNoContent, nil)

	t.Run("the post comes back with its tree", func(t *testing.T) {
		c := c.as(t)
		var view discussion.PostView
		c.expect(http.MethodGet, "/posts/"+post.ID, nil, http.StatusOK, &view)

		if view.ID != post.ID || view.AuthorID != "alice" || view.Likes != 2 {
			t.Errorf("view = %+v with %d likes, want the post by alice with 2", view.Post, view.Likes)
		}
		if len(view.Comments) != 2 {
			t.Fatalf("top-level comments = %d, want 2", len(view.Comments))
		}
		if view.Comments[0].ID != c1.ID || view.Comments[0].Likes != 1 {
			t.Errorf("first comment = %q with %d likes, want %q with 1",
				view.Comments[0].ID, view.Comments[0].Likes, c1.ID)
		}
		if view.Comments[0].Replies[0].Replies[0].ID != c3.ID {
			t.Error("c3 is not at level 3")
		}
	})

	t.Run("engagement sums up the thread", func(t *testing.T) {
		c := c.as(t)
		var got discussion.Engagement
		c.expect(http.MethodGet, "/posts/"+post.ID+"/engagement", nil, http.StatusOK, &got)

		// bob, carol and dave commented; erin only liked; alice wrote the post.
		want := discussion.Engagement{
			PostID: post.ID, PostLikes: 2, CommentLikes: 2, CommentCount: 4,
			DistinctEngagedUsers: 4,
			CommentsByDepth:      map[int]int{1: 2, 2: 1, 3: 1},
			LongestReplyChain:    3,
		}
		if got.PostLikes != want.PostLikes || got.CommentLikes != want.CommentLikes ||
			got.CommentCount != want.CommentCount ||
			got.DistinctEngagedUsers != want.DistinctEngagedUsers ||
			got.LongestReplyChain != want.LongestReplyChain {
			t.Errorf("engagement = %+v, want %+v", got, want)
		}
		for level, count := range want.CommentsByDepth {
			if got.CommentsByDepth[level] != count {
				t.Errorf("level %d has %d comments, want %d", level, got.CommentsByDepth[level], count)
			}
		}
	})

	t.Run("unlike takes one like away", func(t *testing.T) {
		c := c.as(t)
		c.expect(http.MethodDelete, "/posts/"+post.ID+"/likes/erin", nil, http.StatusNoContent, nil)

		var view discussion.PostView
		c.expect(http.MethodGet, "/posts/"+post.ID, nil, http.StatusOK, &view)
		if view.Likes != 1 {
			t.Errorf("likes = %d, want 1", view.Likes)
		}
	})
}

func TestLikeAndUnlikeRepeat(t *testing.T) {
	c := newClient(t)
	post := c.createPost("alice")
	var view discussion.PostView

	for range 3 {
		c.expect(http.MethodPut, "/posts/"+post.ID+"/likes/bob", nil, http.StatusNoContent, nil)
	}
	c.expect(http.MethodGet, "/posts/"+post.ID, nil, http.StatusOK, &view)
	if view.Likes != 1 {
		t.Fatalf("likes after three PUTs = %d, want 1", view.Likes)
	}

	for range 2 {
		c.expect(http.MethodDelete, "/posts/"+post.ID+"/likes/bob", nil, http.StatusNoContent, nil)
	}
	c.expect(http.MethodGet, "/posts/"+post.ID, nil, http.StatusOK, &view)
	if view.Likes != 0 {
		t.Fatalf("likes after two DELETEs = %d, want 0", view.Likes)
	}
}

func TestIdempotencyKeyOnCreates(t *testing.T) {
	c := newClient(t)

	t.Run("a repeated post returns 200 and the first post", func(t *testing.T) {
		c := c.as(t)
		body := map[string]string{"author_id": "alice", "title": "t", "body": "b"}
		headers := map[string]string{"Idempotency-Key": "abc"}

		resp, raw := c.send(http.MethodPost, "/posts", body, headers)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("first call status = %d, want 201 (%s)", resp.StatusCode, raw)
		}
		var first discussion.Post
		json.Unmarshal(raw, &first)

		resp, raw = c.send(http.MethodPost, "/posts", body, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("repeat status = %d, want 200 (%s)", resp.StatusCode, raw)
		}
		var again discussion.Post
		json.Unmarshal(raw, &again)
		if again.ID != first.ID {
			t.Errorf("repeat returned %q, want %q", again.ID, first.ID)
		}
	})

	t.Run("a repeated comment returns 200 and the first comment", func(t *testing.T) {
		c := c.as(t)
		post := c.createPost("alice")
		body := map[string]string{"author_id": "bob", "body": "hi"}
		headers := map[string]string{"Idempotency-Key": "xyz"}
		path := "/posts/" + post.ID + "/comments"

		_, raw := c.send(http.MethodPost, path, body, headers)
		var first discussion.Comment
		json.Unmarshal(raw, &first)

		resp, raw := c.send(http.MethodPost, path, body, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("repeat status = %d, want 200 (%s)", resp.StatusCode, raw)
		}
		var again discussion.Comment
		json.Unmarshal(raw, &again)
		if again.ID != first.ID {
			t.Errorf("repeat returned %q, want %q", again.ID, first.ID)
		}
	})

	t.Run("without a key a repeat makes a second post", func(t *testing.T) {
		c := c.as(t)
		a := c.createPost("alice")
		b := c.createPost("alice")
		if a.ID == b.ID {
			t.Error("two calls without a key returned the same post")
		}
	})
}

func TestEmptyCollectionsAreNotNull(t *testing.T) {
	c := newClient(t)
	post := c.createPost("alice")

	var view struct {
		Comments []json.RawMessage `json:"comments"`
	}
	c.expect(http.MethodGet, "/posts/"+post.ID, nil, http.StatusOK, &view)
	if view.Comments == nil {
		t.Error("comments is null, want []")
	}

	var engagement struct {
		ByDepth map[string]int `json:"comments_by_depth"`
	}
	c.expect(http.MethodGet, "/posts/"+post.ID+"/engagement", nil, http.StatusOK, &engagement)
	if engagement.ByDepth == nil {
		t.Error("comments_by_depth is null, want {}")
	}
}

func TestErrorResponses(t *testing.T) {
	c := newClient(t)
	post := c.createPost("alice")

	tests := []struct {
		name   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{
			"no author", http.MethodPost, "/posts",
			map[string]string{"title": "t", "body": "b"},
			http.StatusBadRequest, "invalid_argument",
		},
		{
			"title over the limit", http.MethodPost, "/posts",
			map[string]string{"author_id": "a", "title": strings.Repeat("x", 201), "body": "b"},
			http.StatusBadRequest, "invalid_argument",
		},
		{
			"misspelled field", http.MethodPost, "/posts",
			map[string]string{"athor_id": "alice", "title": "t", "body": "b"},
			http.StatusBadRequest, "invalid_json",
		},
		{"post does not exist", http.MethodGet, "/posts/nope", nil, http.StatusNotFound, "not_found"},
		{
			"engagement for a missing post", http.MethodGet, "/posts/nope/engagement", nil,
			http.StatusNotFound, "not_found",
		},
		{
			"comment on a missing post", http.MethodPost, "/posts/nope/comments",
			map[string]string{"author_id": "bob", "body": "hi"},
			http.StatusNotFound, "not_found",
		},
		{
			"reply to a missing comment", http.MethodPost, "/posts/" + post.ID + "/comments",
			map[string]string{"author_id": "bob", "body": "hi", "parent_id": "nope"},
			http.StatusNotFound, "not_found",
		},
		{
			"like a missing comment", http.MethodPut, "/comments/nope/likes/bob", nil,
			http.StatusNotFound, "not_found",
		},
		{
			"unlike a missing post", http.MethodDelete, "/posts/nope/likes/bob", nil,
			http.StatusNotFound, "not_found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			c.expect(tt.method, tt.path, tt.body, tt.status, &body)

			if body.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.code)
			}
			if body.Error.Message == "" {
				t.Error("message is empty")
			}
		})
	}
}

func TestUnusableRequests(t *testing.T) {
	c := newClient(t)
	oversized, _ := json.Marshal(map[string]string{
		"author_id": "alice", "title": "t", "body": strings.Repeat("x", 2<<20),
	})

	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{"broken JSON", "application/json", "{not json", http.StatusBadRequest},
		{"empty body", "application/json", "", http.StatusBadRequest},
		{"not JSON at all", "text/plain", "{}", http.StatusUnsupportedMediaType},
		{"over the size limit", "application/json", string(oversized), http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(c.base+prefix+"/posts", tt.contentType, strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}

	t.Run("wrong method on a known path", func(t *testing.T) {
		c := c.as(t)
		req, _ := http.NewRequest(http.MethodDelete, c.base+prefix+"/posts", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
}

func TestRoutingOutsideTheVersionPrefix(t *testing.T) {
	c := newClient(t)

	t.Run("Location points inside the prefix", func(t *testing.T) {
		c := c.as(t)
		resp, _ := c.send(http.MethodPost, "/posts",
			map[string]string{"author_id": "alice", "title": "t", "body": "b"}, nil)
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, prefix+"/posts/") {
			t.Errorf("Location = %q, want it under %s/posts/", loc, prefix)
		}
	})

	t.Run("unversioned and unknown versions are not served", func(t *testing.T) {
		c := c.as(t)
		for _, path := range []string{"/posts", "/v2/posts"} {
			resp, err := http.Get(c.base + path)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
			}
		}
	})

	t.Run("health and the spec sit outside the prefix", func(t *testing.T) {
		c := c.as(t)
		for _, path := range []string{"/healthz", "/openapi.yaml"} {
			resp, err := http.Get(c.base + path)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			}
			if path == "/openapi.yaml" && !bytes.Contains(body, []byte("openapi:")) {
				t.Error("the spec does not look like OpenAPI")
			}
		}
	})
}

func TestRequestID(t *testing.T) {
	c := newClient(t)

	t.Run("one is made when the caller sends none", func(t *testing.T) {
		c := c.as(t)
		resp, raw := c.send(http.MethodGet, "/posts/nope", nil, nil)

		id := resp.Header.Get("X-Request-Id")
		if id == "" {
			t.Fatal("X-Request-Id is empty")
		}

		var body struct {
			Error struct {
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Error.RequestID != id {
			t.Errorf("body has %q, header has %q", body.Error.RequestID, id)
		}
	})

	t.Run("a usable one from the caller is kept", func(t *testing.T) {
		c := c.as(t)
		resp, _ := c.send(http.MethodGet, "/posts/nope", nil,
			map[string]string{"X-Request-Id": "trace-abc-123"})
		if got := resp.Header.Get("X-Request-Id"); got != "trace-abc-123" {
			t.Errorf("X-Request-Id = %q, want the caller's value", got)
		}
	})

	t.Run("an unusable one is replaced", func(t *testing.T) {
		c := c.as(t)
		for name, bad := range map[string]string{
			"newline":  "abc\ndef",
			"too long": strings.Repeat("x", 65),
		} {
			t.Run(name, func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodGet, c.base+prefix+"/posts/nope", nil)
				req.Header.Set("X-Request-Id", bad)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					// net/http refused to send it, which is fine too.
					return
				}
				defer resp.Body.Close()
				if got := resp.Header.Get("X-Request-Id"); got == bad {
					t.Errorf("X-Request-Id = %q, want a replacement", got)
				}
			})
		}
	})
}

func TestPagingOverHTTP(t *testing.T) {
	c := newClient(t)
	post := c.createPost("alice")

	var created []string
	for i := range 5 {
		comment := c.createComment(post.ID, fmt.Sprintf("user-%d", i), nil)
		created = append(created, comment.ID)
		c.createComment(post.ID, "replier", &comment.ID)
	}

	t.Run("limit and cursor walk the thread", func(t *testing.T) {
		c := c.as(t)
		var got []string
		path := "/posts/" + post.ID + "?limit=2"
		for range 5 {
			var view discussion.PostView
			c.expect(http.MethodGet, path, nil, http.StatusOK, &view)

			for _, comment := range view.Comments {
				got = append(got, comment.ID)
				if comment.ReplyCount != 1 || len(comment.Replies) != 1 {
					t.Errorf("comment %s lost its reply", comment.ID)
				}
			}
			if view.Page.TotalTopLevel != 5 {
				t.Errorf("total = %d, want 5", view.Page.TotalTopLevel)
			}
			if !view.Page.HasMore {
				break
			}
			path = "/posts/" + post.ID + "?limit=2&cursor=" + view.Page.NextCursor
		}
		if !slices.Equal(got, created) {
			t.Errorf("paged through %v, want %v", got, created)
		}
	})

	t.Run("max_depth trims replies and reply_count shows what is left", func(t *testing.T) {
		c := c.as(t)
		var view discussion.PostView
		c.expect(http.MethodGet, "/posts/"+post.ID+"?max_depth=1", nil, http.StatusOK, &view)

		for _, comment := range view.Comments {
			if len(comment.Replies) != 0 || comment.ReplyCount != 1 {
				t.Errorf("comment %s has %d replies and reports %d, want 0 and 1",
					comment.ID, len(comment.Replies), comment.ReplyCount)
			}
		}
	})

	t.Run("engagement ignores paging", func(t *testing.T) {
		c := c.as(t)
		var got discussion.Engagement
		c.expect(http.MethodGet, "/posts/"+post.ID+"/engagement?limit=1", nil, http.StatusOK, &got)
		if got.CommentCount != 10 {
			t.Errorf("CommentCount = %d, want 10", got.CommentCount)
		}
	})

	t.Run("bad paging parameters are rejected", func(t *testing.T) {
		c := c.as(t)
		for _, query := range []string{"?limit=abc", "?limit=0", "?limit=101", "?max_depth=-1"} {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			c.expect(http.MethodGet, "/posts/"+post.ID+query, nil, http.StatusBadRequest, &body)
			if body.Error.Code != "invalid_argument" {
				t.Errorf("%s gave code %q, want invalid_argument", query, body.Error.Code)
			}
		}
	})
}
