package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// Model calls take 30-90 s. 150 s leaves room for a slow answer plus the
// client's own retries, and still ends before the 180 s Lambda timeout, so
// the user gets a proper error instead of a dropped connection.
const modelTimeout = 150 * time.Second

func modelNotConfigured(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "model_not_configured",
		"no model is configured on this server; enter the data manually instead", nil)
}

// modelCallsPerIPPerDay caps model operations per client IP (an IPv6 /64
// counts as one client) per UTC day. Ten covers three full runs through the
// workflow, and stops a single client from using up the global quota below
// and locking everyone else out until midnight.
const modelCallsPerIPPerDay = 10

// takeModelQuota uses one unit of today's (UTC) model budget. One unit is one
// model operation (reading a document, or suggesting codes for an invoice),
// which may make up to two Claude requests if the first answer has to be
// corrected.
//
// Two daily counters are taken, both kept in the store so that every server
// instance shares them (the in-memory rate limiter can't do that on Lambda):
//
//  1. this client's own count, so one client can't use up the day for
//     everyone (429 rate_limited);
//  2. the global count, which caps the total number of model operations a
//     day whatever happens (429 model_quota_exhausted).
//
// The per-client count is taken first, so a refused client never uses a unit
// of the global budget. A unit is not given back if the call then fails: the
// model may already have been paid for. The cost of each operation is capped
// separately, by the model client refusing documents that are too large.
func (s *Server) takeModelQuota(w http.ResponseWriter, r *http.Request) bool {
	now := s.now().UTC()
	date := now.Format(time.DateOnly)

	ok, err := s.store.TakeQuota(r.Context(), "model_calls_ip:"+clientIP(r.RemoteAddr)+":"+date, modelCallsPerIPPerDay)
	if err != nil {
		quotaError(w, r, err)
		return false
	}
	if !ok {
		retryAtMidnight(w, now)
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"this address has used today's model calls for this demo; try again after midnight UTC", nil)
		return false
	}

	ok, err = s.store.TakeQuota(r.Context(), "model_calls:"+date, s.modelQuota)
	if err != nil {
		quotaError(w, r, err)
		return false
	}
	if !ok {
		retryAtMidnight(w, now)
		writeError(w, http.StatusTooManyRequests, "model_quota_exhausted",
			"today's model budget for this demo is used up; try again after midnight UTC", nil)
		return false
	}
	return true
}

func quotaError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("take model quota", "request_id", requestIDFrom(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// retryAtMidnight tells the client when the daily counters start again.
func retryAtMidnight(w http.ResponseWriter, now time.Time) {
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(midnight.Sub(now).Seconds()))))
}

// modelLabel names the model that answered, falling back to the configured
// name if the run didn't say (e.g. the call failed before any answer).
func (s *Server) modelLabel(run domain.ModelRun) string {
	if run.Model != "" {
		return run.Model
	}
	return s.modelName
}

// runDetail is the audit detail for a model call: names and counts only,
// never the prompt or the answer.
func (s *Server) runDetail(run domain.ModelRun) map[string]string {
	return map[string]string{
		"model":         s.modelLabel(run),
		"attempts":      strconv.Itoa(run.Attempts),
		"input_tokens":  strconv.FormatInt(run.InputTokens, 10),
		"output_tokens": strconv.FormatInt(run.OutputTokens, 10),
	}
}

// modelFailed answers a failed model call and audits it with a short reason
// code. The error text itself is not logged: errors from a model client can
// quote the model's output, which may contain document contents.
func (s *Server) modelFailed(w http.ResponseWriter, r *http.Request, shipmentID, action, entityType, entityID string, run domain.ModelRun, err error) {
	status, code, msg, reason := http.StatusBadGateway, "model_failed", "the model could not be reached; please try again later", "error"
	var details any
	var invalid *ai.InvalidOutputError
	switch {
	case errors.Is(err, ai.ErrRefused):
		status, code, msg, reason = http.StatusUnprocessableEntity, "model_refused", "the model declined to process this request", "refused"
	case errors.Is(err, ai.ErrTooLarge):
		// Retrying would fail the same way, so tell the user what to do instead.
		status, code, msg, reason = http.StatusUnprocessableEntity, "document_too_large",
			"the document is too large for the model to read in one pass; split it into smaller files or enter the data manually", "too_large"
	case errors.As(err, &invalid):
		status, code, msg, reason = http.StatusUnprocessableEntity, "validation_failed",
			"the model's answer still did not match the schema after retries", "invalid_output"
		details, run = invalid.Errs, invalid.Run
	case errors.Is(err, context.DeadlineExceeded):
		msg, reason = "the model took too long to answer; please try again later", "timeout"
	case errors.Is(err, ai.ErrUnavailable):
		reason = "unavailable"
	}

	detail := s.runDetail(run)
	detail["reason"] = reason
	slog.Warn("model call failed", "request_id", requestIDFrom(r.Context()),
		"reason", reason, "model", detail["model"], "attempts", run.Attempts)
	s.audit(r, shipmentID, action, entityType, entityID, detail)
	writeError(w, status, code, msg, details)
}
