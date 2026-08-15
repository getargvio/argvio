package otlp

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// grpcAPIKey reads APIKeyHeader out of incoming gRPC metadata.
func grpcAPIKey(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(APIKeyHeader)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// grpcAuthError maps this package's auth/rate-limit sentinel errors to the
// gRPC status codes the OTLP spec expects clients to handle distinctly
// (Unauthenticated vs ResourceExhausted vs PermissionDenied).
func grpcAuthError(err error) error {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "invalid or missing API key")
	case errors.Is(err, ErrTenantSuspended):
		return status.Error(codes.PermissionDenied, "tenant suspended")
	case errors.Is(err, ErrRateLimited):
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// grpcProcessError maps a Process* structural error (e.g. batch too large,
// unsupported signal type) to InvalidArgument; anything else is an
// unexpected internal failure whose detail must not leak to the client.
func grpcProcessError(err error) error {
	if errors.Is(err, ErrBatchTooLarge) || errors.Is(err, ErrUnknownSignalType) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.Internal, "internal error")
}

// MetricsGRPCService implements the OTLP MetricsService gRPC API.
type MetricsGRPCService struct {
	pmetricotlp.UnimplementedGRPCServer
	*core
}

func (s *MetricsGRPCService) Export(ctx context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	raw, _ := req.MarshalProto() // size estimate for byte-rate limiting; see docs/architecture.md
	resolved, err := s.authenticate(ctx, grpcAPIKey(ctx), len(raw))
	if err != nil {
		return pmetricotlp.ExportResponse{}, grpcAuthError(err)
	}

	pipeline := s.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessMetrics(ctx, req.Metrics())
	if err != nil {
		return pmetricotlp.ExportResponse{}, grpcProcessError(err)
	}

	resp := pmetricotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedDataPoints(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	return resp, nil
}

// LogsGRPCService implements the OTLP LogsService gRPC API.
type LogsGRPCService struct {
	plogotlp.UnimplementedGRPCServer
	*core
}

func (s *LogsGRPCService) Export(ctx context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	raw, _ := req.MarshalProto()
	resolved, err := s.authenticate(ctx, grpcAPIKey(ctx), len(raw))
	if err != nil {
		return plogotlp.ExportResponse{}, grpcAuthError(err)
	}

	pipeline := s.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessLogs(ctx, req.Logs())
	if err != nil {
		return plogotlp.ExportResponse{}, grpcProcessError(err)
	}

	resp := plogotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedLogRecords(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	return resp, nil
}

// TracesGRPCService implements the OTLP TraceService gRPC API.
type TracesGRPCService struct {
	ptraceotlp.UnimplementedGRPCServer
	*core
}

func (s *TracesGRPCService) Export(ctx context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	raw, _ := req.MarshalProto()
	resolved, err := s.authenticate(ctx, grpcAPIKey(ctx), len(raw))
	if err != nil {
		return ptraceotlp.ExportResponse{}, grpcAuthError(err)
	}

	pipeline := s.newPipeline(resolved)
	_, rejected, sample, err := pipeline.ProcessTraces(ctx, req.Traces())
	if err != nil {
		return ptraceotlp.ExportResponse{}, grpcProcessError(err)
	}

	resp := ptraceotlp.NewExportResponse()
	if rejected > 0 {
		resp.PartialSuccess().SetRejectedSpans(rejected)
		resp.PartialSuccess().SetErrorMessage(sample)
	}
	return resp, nil
}

// RegisterGRPC wires all three OTLP signal services onto gs.
func (s *Server) RegisterGRPC(gs *grpc.Server) {
	pmetricotlp.RegisterGRPCServer(gs, &MetricsGRPCService{core: s.core})
	plogotlp.RegisterGRPCServer(gs, &LogsGRPCService{core: s.core})
	ptraceotlp.RegisterGRPCServer(gs, &TracesGRPCService{core: s.core})
}
