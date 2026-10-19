package storage

import (
	"strings"
	"testing"
)

func TestErrorSample(t *testing.T) {
	long := "open /home/alice/" + strings.Repeat("x", 2*ErrorSampleMaxLen) + " failed"
	source, msg, top := errorSample(&long, nil)
	if source != "message" || top != nil {
		t.Errorf("source %q top frame %v, want message-only", source, top)
	}
	if msg == nil || *msg != "open <path> failed" {
		t.Errorf("message = %v, want the path normalized away", msg)
	}

	long = strings.Repeat("y ", ErrorSampleMaxLen)
	if _, msg, _ = errorSample(&long, nil); msg == nil || len([]rune(*msg)) != ErrorSampleMaxLen {
		t.Errorf("sample not truncated to %d characters: %v", ErrorSampleMaxLen, msg)
	}

	if source, msg, top = errorSample(nil, nil); source != "" || msg != nil || top != nil {
		t.Errorf("empty event: got %q %v %v", source, msg, top)
	}
}
