package otlp

import "errors"

// ErrBatchTooLarge is a whole-request rejection (not partial success): the
// batch itself is oversized, so there's no principled subset to accept.
// gRPC/HTTP handlers map this to InvalidArgument/400.
var ErrBatchTooLarge = errors.New("otlp: batch exceeds max_batch_size")

// ErrUnknownSignalType is returned when the active allowlist schema has
// disabled a signal type entirely (schema.SupportedSignalTypes) — defense
// in depth on top of the fact that each gRPC/HTTP route is already
// signal-specific.
var ErrUnknownSignalType = errors.New("otlp: signal type not supported")
