// Package httpapi exposes the discussion service over HTTP and JSON.
package httpapi

import (
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aliassar/discussion-service/internal/discussion"
)

const basePath = "/v1"

const idempotencyHeader = "Idempotency-Key"

//go:embed openapi.yaml
var openAPISpec []byte

type API struct {
	svc *discussion.Service
	log *slog.Logger
}

func NewRouter(svc *discussion.Service, log *slog.Logger) http.Handler {
	a := &API{svc: svc, log: log}

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /posts", a.createPost)
	v1.HandleFunc("GET /posts/{postID}", a.getPost)
	v1.HandleFunc("GET /posts/{postID}/engagement", a.getEngagement)
	v1.HandleFunc("POST /posts/{postID}/comments", a.createComment)

	v1.HandleFunc("PUT /posts/{postID}/likes/{userID}", a.like(discussion.TargetPost))
	v1.HandleFunc("DELETE /posts/{postID}/likes/{userID}", a.unlike(discussion.TargetPost))
	v1.HandleFunc("PUT /comments/{commentID}/likes/{userID}", a.like(discussion.TargetComment))
	v1.HandleFunc("DELETE /comments/{commentID}/likes/{userID}", a.unlike(discussion.TargetComment))

	mux := http.NewServeMux()
	mux.Handle(basePath+"/", http.StripPrefix(basePath, v1))
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openAPISpec)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Order matters: the request id must exist before anything logs, and panic
	// recovery sits inside the access log so a panicking request is still logged.
	return withRequestID(logRequests(log, recoverPanics(log, mux)))
}

func (a *API) createPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthorID string `json:"author_id"`
		Title    string `json:"title"`
		Body     string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}

	post, created, err := a.svc.CreatePost(r.Context(), discussion.CreatePostInput{
		AuthorID:       req.AuthorID,
		Title:          req.Title,
		Body:           req.Body,
		IdempotencyKey: r.Header.Get(idempotencyHeader),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}

	w.Header().Set("Location", basePath+"/posts/"+post.ID)
	writeJSON(w, createdStatus(created), post)
}

func (a *API) createComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthorID string  `json:"author_id"`
		Body     string  `json:"body"`
		ParentID *string `json:"parent_id"`
	}
	if !decode(w, r, &req) {
		return
	}

	comment, created, err := a.svc.CreateComment(r.Context(), discussion.CreateCommentInput{
		PostID:         r.PathValue("postID"),
		ParentID:       req.ParentID,
		AuthorID:       req.AuthorID,
		Body:           req.Body,
		IdempotencyKey: r.Header.Get(idempotencyHeader),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, createdStatus(created), comment)
}

func (a *API) getPost(w http.ResponseWriter, r *http.Request) {
	page, err := pageFromQuery(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	view, err := a.svc.PostView(r.Context(), r.PathValue("postID"), page)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func pageFromQuery(r *http.Request) (discussion.Page, error) {
	// An absent parameter parses as 0, which the service treats as "default";
	// an explicit value below the minimum is rejected instead.
	limit, err := intParam(r, "limit", 1)
	if err != nil {
		return discussion.Page{}, err
	}
	maxDepth, err := intParam(r, "max_depth", 0)
	if err != nil {
		return discussion.Page{}, err
	}
	return discussion.Page{
		Limit:    limit,
		MaxDepth: maxDepth,
		Cursor:   r.URL.Query().Get("cursor"),
	}, nil
}

func intParam(r *http.Request, name string, min int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &discussion.ValidationError{Field: name, Message: "must be a whole number"}
	}
	if v < min {
		return 0, &discussion.ValidationError{
			Field:   name,
			Message: fmt.Sprintf("must be %d or more", min),
		}
	}
	return v, nil
}

func (a *API) getEngagement(w http.ResponseWriter, r *http.Request) {
	engagement, err := a.svc.Engagement(r.Context(), r.PathValue("postID"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, engagement)
}

func (a *API) like(kind discussion.TargetType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.svc.Like(r.Context(), target(kind, r), r.PathValue("userID")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) unlike(kind discussion.TargetType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.svc.Unlike(r.Context(), target(kind, r), r.PathValue("userID")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func target(kind discussion.TargetType, r *http.Request) discussion.Target {
	if kind == discussion.TargetPost {
		return discussion.Target{Type: kind, ID: r.PathValue("postID")}
	}
	return discussion.Target{Type: kind, ID: r.PathValue("commentID")}
}

func createdStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}
