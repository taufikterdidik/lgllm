package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// errorEnvelope mirrors the [OI] error shape so existing clients parse lgllm
// errors without special cases.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// writeJSON encodes v and writes it with the given status.
//
// The payload is encoded before the status line is sent: an encoding failure
// must not leave a half-written 200 on the wire.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Nothing sensible can be reported about the failure itself here; the
		// caller's logger is not reachable, so fall back to the process logger.
		slog.Error("failed to encode response", "err", err)
		http.Error(w, `{"error":{"message":"internal error","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	buf = append(buf, '\n')

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeJSONError writes an [OI]-shaped error body.
func writeJSONError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: message, Type: kind}})
}

// writeBadRequest reports a request-decoding problem.
//
// http.MaxBytesReader reports overflow through the decode error, so this also
// turns an oversized body into 413 rather than a misleading 400.
func writeBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		LoggerFrom(r.Context()).Warn("request body too large", "limit", maxErr.Limit)
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"request body exceeds "+strconv.FormatInt(maxErr.Limit, 10)+" bytes", "request_too_large")
		return
	}
	LoggerFrom(r.Context()).Warn("invalid request body", "err", err)
	writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error")
}
