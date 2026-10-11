package apiserve

import (
	"context"
	"net/http"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// AccessRecord is one structured access log line. It doubles as the
// "usage/audit" half of the plan's observability requirement: without topic
// parsing there is no way to bill, throttle, and explain a request after the
// fact.
type AccessRecord struct {
	RequestID      string
	Method         string
	Path           string
	APIKeyID       string
	AgentID        string
	ConversationID string
	StatusCode     int
	DurationMS     int64
	InputTokens    int
	OutputTokens   int
}

// withAccessLog emits one structured record per request, including rejected ones.
//
// The record is a pointer parked on the context before calling next: middlewares
// and handlers mutate that same object as they learn more, so the outer function
// sees the enriched values. (Passing r.WithContext downstream means the outer
// *http.Request never sees what the inner middleware added, so a copy of the
// record would lose api_key_id and agent_id.)
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Resolve the request id ONCE and pin it on the request. requestIDFrom
		// falls back to a fresh random id on *every* call when the header is
		// absent, so without this the access log, the error envelope and
		// usage_records would each carry a different id and could not be
		// correlated — which is the whole point of logging one.
		requestID := requestIDFrom(r)
		r.Header.Set("X-Request-Id", requestID)

		record := &AccessRecord{
			RequestID: requestID,
			Method:    r.Method,
			Path:      r.URL.Path,
		}
		r = r.WithContext(context.WithValue(r.Context(), accessRecordKey{}, record))

		next.ServeHTTP(recorder, r)

		record.StatusCode = recorder.status
		record.DurationMS = time.Since(started).Milliseconds()
		s.emitAccessLog(*record)
	})
}

// emitAccessLog writes the record through the injectable sink.
func (s *Server) emitAccessLog(rec AccessRecord) {
	if s.accessLog != nil {
		s.accessLog(rec)
		return
	}
	logger.InfoCF("apiserve", "request", map[string]any{
		"request_id":      rec.RequestID,
		"method":          rec.Method,
		"path":            rec.Path,
		"api_key_id":      rec.APIKeyID,
		"agent_id":        rec.AgentID,
		"conversation_id": rec.ConversationID,
		"status":          rec.StatusCode,
		"duration_ms":     rec.DurationMS,
		// spec §7.3 names `usage` as part of the request log line. It mirrors the
		// response block so a log reader does not have to join usage_records to
		// answer "how much did this request cost".
		"usage": map[string]any{
			"input_tokens":  rec.InputTokens,
			"output_tokens": rec.OutputTokens,
		},
	})
}

type accessRecordKey struct{}

// accessRecordFrom returns the mutable access record of the current request.
func accessRecordFrom(ctx context.Context) (*AccessRecord, bool) {
	rec, ok := ctx.Value(accessRecordKey{}).(*AccessRecord)
	return rec, ok
}

// markAccessRecord enriches the access record with per-request dimensions.
// It is a no-op when the access log middleware is not in the chain (tests that
// call handlers directly).
func markAccessRecord(r *http.Request, agentID, conversationID string) {
	if rec, ok := accessRecordFrom(r.Context()); ok {
		rec.AgentID = agentID
		rec.ConversationID = conversationID
	}
}

// markAccessUsage records what the turn actually spent on the request log line.
func markAccessUsage(r *http.Request, inputTokens, outputTokens int) {
	if rec, ok := accessRecordFrom(r.Context()); ok {
		rec.InputTokens = inputTokens
		rec.OutputTokens = outputTokens
	}
}

// statusRecorder captures the response status code.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}
