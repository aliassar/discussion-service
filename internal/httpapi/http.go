package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aliassar/discussion-service/internal/discussion"
)

const maxRequestBody = 1 << 20 // 1 MiB

const requestIDHeader = "X-Request-Id"

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestID = RequestIDFromContext(r.Context())
	writeJSON(w, status, body)
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *discussion.ValidationError
	switch {
	case errors.Is(err, discussion.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.As(err, &invalid):
		writeError(w, r, http.StatusBadRequest, "invalid_argument", invalid.Error())
	case errors.Is(err, context.Canceled):
		w.WriteHeader(499) // client closed request (nginx convention)
	default:
		a.log.ErrorContext(r.Context(), "unhandled error",
			slog.String("request_id", RequestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err))
		writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
	}
}

// decode writes the error response itself and returns false when the body is
// unusable.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("body must be at most %d bytes", maxRequestBody))
			return false
		}
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

type contextKey int

const requestIDKey contextKey = iota

func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !usableRequestID(id) {
			id = randomHex()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// usableRequestID admits only bounded, printable ASCII: the value reaches the
// logs, and a newline in it would let a caller forge log lines.
func usableRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func randomHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		log.InfoContext(r.Context(), "request",
			slog.String("request_id", RequestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("took", time.Since(start)))
	})
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { // intentional abort; propagate
				panic(p)
			}
			log.ErrorContext(r.Context(), "panic serving request",
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())))
			writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
