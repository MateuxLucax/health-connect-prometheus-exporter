package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/MateuxLucax/health-connect-prometheus-exporter/internal/remotewrite"
)

const (
	maxBodyBytes     = 8 << 20
	maxLabels        = 16
	maxLabelValueLen = 1024
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// sample is one sample as the app sends it. Value is a pointer so that a
// missing value is told apart from 0.
type sample struct {
	Metric      string            `json:"metric"`
	Labels      map[string]string `json:"labels"`
	Value       *float64          `json:"value"`
	TimestampMs int64             `json:"timestamp_ms"`
}

type request struct {
	Samples []sample `json:"samples"`
}

func decodeSamples(body io.Reader) ([]sample, error) {
	var req request
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: unexpected data after the top-level value")
	}
	for i, s := range req.Samples {
		if !metricNameRE.MatchString(s.Metric) {
			return nil, fmt.Errorf("sample %d: invalid metric name %q", i, s.Metric)
		}
		if len(s.Labels) > maxLabels {
			return nil, fmt.Errorf("sample %d: more than %d labels", i, maxLabels)
		}
		for name, value := range s.Labels {
			if !labelNameRE.MatchString(name) || strings.HasPrefix(name, "__") {
				return nil, fmt.Errorf("sample %d: invalid label name %q", i, name)
			}
			if len(value) > maxLabelValueLen {
				return nil, fmt.Errorf("sample %d: label %q value longer than %d bytes", i, name, maxLabelValueLen)
			}
		}
		if s.Value == nil {
			return nil, fmt.Errorf("sample %d: missing value", i)
		}
		if s.TimestampMs <= 0 {
			return nil, fmt.Errorf("sample %d: timestamp_ms must be > 0", i)
		}
	}
	return req.Samples, nil
}

// toRemoteWrite converts a validated sample (Value is not nil).
func (s sample) toRemoteWrite() remotewrite.Sample {
	return remotewrite.Sample{Metric: s.Metric, Labels: s.Labels, Value: *s.Value, TimestampMs: s.TimestampMs}
}
