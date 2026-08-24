package reporter

import (
	"context"
	"encoding/json"
	"io"
	"sync"
)

type JSONReporter struct {
	encoder *json.Encoder
	mu      sync.Mutex
}

func NewJSON(output io.Writer) *JSONReporter {
	return &JSONReporter{encoder: json.NewEncoder(output)}
}

func (r *JSONReporter) Report(_ context.Context, status Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.encoder.Encode(status)
}
