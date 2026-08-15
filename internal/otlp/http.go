package otlp

import (
	"errors"
	"io"
	"net/http"
	"strings"

	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// maxHTTPBodyBytes bounds a single OTLP/HTTP request body regardless of
// item-count bounds (Bounds.MaxBatchSize) — a batch within the item-count
// limit can still be made huge via oversized individual field values before
// per-attribute length checks run, so the body itself needs its own cap.
const maxHTTPBodyBytes = 32 << 20 // 32 MiB

const (
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
)

// httpAPIKey reads APIKeyHeader, falling back to a standard
// "Authorization: Bearer <key>" header for OTel exporter configs that only
// support the standard auth header.
func httpAPIKey(r *http.Request) string {
	if k := r.Header.Get(APIKeyHeader); k != "" {
		return k
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func httpStatusForAuthError(err error) int {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrTenantSuspended):
		return http.StatusForbidden
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func httpStatusForProcessError(err error) int {
	if errors.Is(err, ErrBatchTooLarge) || errors.Is(err, ErrUnknownSignalType) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// writeHTTPError writes an OTLP/HTTP-spec-shaped error body: a
// google.rpc.Status message, protobuf- or JSON-encoded to match the
// request's declared content type (falling back to JSON for anything else,
// e.g. a request with no/unsupported Content-Type).
func writeHTTPError(w http.ResponseWriter, wantProtobuf bool, httpCode int, grpcCode codes.Code, message string) {
	st := &rpcstatus.Status{Code: int32(grpcCode), Message: message} // #nosec G115 -- grpcCode is one of this package's own codes.Code constants (0-16), not client input
	w.WriteHeader(httpCode)
	if wantProtobuf {
		w.Header().Set("Content-Type", contentTypeProtobuf)
		b, err := proto.Marshal(st)
		if err == nil {
			_, _ = w.Write(b)
		}
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	b, err := protojson.Marshal(st)
	if err == nil {
		_, _ = w.Write(b)
	}
}

func negotiateProtobuf(r *http.Request) (wantProtobuf bool, ok bool) {
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, contentTypeProtobuf):
		return true, true
	case strings.HasPrefix(ct, contentTypeJSON):
		return false, true
	default:
		return false, false
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHTTPError(w, false, http.StatusRequestEntityTooLarge, codes.InvalidArgument, "request body exceeds maximum allowed size")
		return nil, false
	}
	return body, true
}

// HTTPHandler returns the OTLP/HTTP mux: POST /v1/metrics, /v1/logs,
// /v1/traces, each accepting application/x-protobuf or application/json
// per the OTLP/HTTP spec, with matching-format responses.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", s.handleMetricsHTTP)
	mux.HandleFunc("POST /v1/logs", s.handleLogsHTTP)
	mux.HandleFunc("POST /v1/traces", s.handleTracesHTTP)
	return mux
}

func (s *Server) handleMetricsHTTP(w http.ResponseWriter, r *http.Request) {
	wantProtobuf, ok := negotiateProtobuf(r)
	if !ok {
		writeHTTPError(w, false, http.StatusUnsupportedMediaType, codes.InvalidArgument, "Content-Type must be application/x-protobuf or application/json")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	req := pmetricotlp.NewExportRequest()
	var err error
	if wantProtobuf {
		err = req.UnmarshalProto(body)
	} else {
		err = req.UnmarshalJSON(body)
	}
	if err != nil {
		writeHTTPError(w, wantProtobuf, http.StatusBadRequest, codes.InvalidArgument, "malformed OTLP metrics payload: "+err.Error())
		return
	}

	resolved, err := s.core.authenticate(r.Context(), httpAPIKey(r), len(body))
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForAuthError(err), codes.Unauthenticated, err.Error())
		return
	}

	pipeline := s.core.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessMetrics(r.Context(), req.Metrics())
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForProcessError(err), codes.InvalidArgument, err.Error())
		return
	}

	resp := pmetricotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedDataPoints(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	writeExportResponse(w, wantProtobuf, resp.MarshalProto, resp.MarshalJSON)
}

func (s *Server) handleLogsHTTP(w http.ResponseWriter, r *http.Request) {
	wantProtobuf, ok := negotiateProtobuf(r)
	if !ok {
		writeHTTPError(w, false, http.StatusUnsupportedMediaType, codes.InvalidArgument, "Content-Type must be application/x-protobuf or application/json")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	req := plogotlp.NewExportRequest()
	var err error
	if wantProtobuf {
		err = req.UnmarshalProto(body)
	} else {
		err = req.UnmarshalJSON(body)
	}
	if err != nil {
		writeHTTPError(w, wantProtobuf, http.StatusBadRequest, codes.InvalidArgument, "malformed OTLP logs payload: "+err.Error())
		return
	}

	resolved, err := s.core.authenticate(r.Context(), httpAPIKey(r), len(body))
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForAuthError(err), codes.Unauthenticated, err.Error())
		return
	}

	pipeline := s.core.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessLogs(r.Context(), req.Logs())
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForProcessError(err), codes.InvalidArgument, err.Error())
		return
	}

	resp := plogotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedLogRecords(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	writeExportResponse(w, wantProtobuf, resp.MarshalProto, resp.MarshalJSON)
}

func (s *Server) handleTracesHTTP(w http.ResponseWriter, r *http.Request) {
	wantProtobuf, ok := negotiateProtobuf(r)
	if !ok {
		writeHTTPError(w, false, http.StatusUnsupportedMediaType, codes.InvalidArgument, "Content-Type must be application/x-protobuf or application/json")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	req := ptraceotlp.NewExportRequest()
	var err error
	if wantProtobuf {
		err = req.UnmarshalProto(body)
	} else {
		err = req.UnmarshalJSON(body)
	}
	if err != nil {
		writeHTTPError(w, wantProtobuf, http.StatusBadRequest, codes.InvalidArgument, "malformed OTLP traces payload: "+err.Error())
		return
	}

	resolved, err := s.core.authenticate(r.Context(), httpAPIKey(r), len(body))
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForAuthError(err), codes.Unauthenticated, err.Error())
		return
	}

	pipeline := s.core.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessTraces(r.Context(), req.Traces())
	if err != nil {
		writeHTTPError(w, wantProtobuf, httpStatusForProcessError(err), codes.InvalidArgument, err.Error())
		return
	}

	resp := ptraceotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedSpans(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	writeExportResponse(w, wantProtobuf, resp.MarshalProto, resp.MarshalJSON)
}

func writeExportResponse(w http.ResponseWriter, wantProtobuf bool, marshalProto func() ([]byte, error), marshalJSON func() ([]byte, error)) {
	var b []byte
	var err error
	if wantProtobuf {
		w.Header().Set("Content-Type", contentTypeProtobuf)
		b, err = marshalProto()
	} else {
		w.Header().Set("Content-Type", contentTypeJSON)
		b, err = marshalJSON()
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
