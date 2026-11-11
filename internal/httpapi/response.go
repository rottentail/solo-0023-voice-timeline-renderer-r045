package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"voice-timeline-renderer/internal/domain"
)

type errorEnvelope struct {
	Error domain.ErrorDetail `json:"error"`
	Job   *domain.RenderJob  `json:"job,omitempty"`
}

func decodeJSON(w http.ResponseWriter, request *http.Request, target any, limit int64) error {
	request.Body = http.MaxBytesReader(w, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			return domain.E(domain.KindTooLarge, "request_too_large", "", "request body exceeds the accepted size")
		}
		return domain.Wrap(domain.KindInvalid, "invalid_json", "", "request body must contain one valid JSON object", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return domain.E(domain.KindInvalid, "invalid_json", "", "request body must contain exactly one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error, job *domain.RenderJob) {
	appErr := domain.AsAppError(err)
	status := statusForKind(appErr.Kind)
	envelope := errorEnvelope{Error: appErr.Detail}
	if job != nil && job.ID != "" {
		envelope.Job = job
	}
	writeJSON(w, status, envelope)
}

func statusForKind(kind domain.ErrorKind) int {
	switch kind {
	case domain.KindInvalid:
		return http.StatusUnprocessableEntity
	case domain.KindConflict, domain.KindUnready:
		return http.StatusConflict
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindGone:
		return http.StatusGone
	case domain.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, request)
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("request panic", "method", request.Method, "path", request.URL.Path, "panic", recovered)
				writeError(w, domain.E(domain.KindInternal, "internal_error", "", "the request could not be completed"), nil)
			}
		}()
		next.ServeHTTP(w, request)
	})
}
