package ingest

import (
	"fmt"
	"strings"
	"testing"
)

func TestDecodeSamples(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"samples":[{"metric":"health_heart_rate_bpm","labels":{"source":"com.sec.android.app.shealth"},"value":72,"timestamp_ms":1792839600000}]}`, true},
		{"empty list", `{"samples":[]}`, true},
		{"colon in metric", `{"samples":[{"metric":"a:b","value":1,"timestamp_ms":1}]}`, true},
		{"not json", `nope`, false},
		{"bad metric", `{"samples":[{"metric":"1abc","value":1,"timestamp_ms":1}]}`, false},
		{"empty metric", `{"samples":[{"metric":"","value":1,"timestamp_ms":1}]}`, false},
		{"bad label", `{"samples":[{"metric":"m","labels":{"a-b":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"colon in label", `{"samples":[{"metric":"m","labels":{"a:b":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"reserved label", `{"samples":[{"metric":"m","labels":{"__name__":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"missing value", `{"samples":[{"metric":"m","timestamp_ms":1}]}`, false},
		{"zero timestamp", `{"samples":[{"metric":"m","value":1,"timestamp_ms":0}]}`, false},
		{"negative timestamp", `{"samples":[{"metric":"m","value":1,"timestamp_ms":-5}]}`, false},
		{"16 labels", `{"samples":[{"metric":"m","labels":{` + specLabels(16, 1) + `},"value":1,"timestamp_ms":1}]}`, true},
		{"17 labels", `{"samples":[{"metric":"m","labels":{` + specLabels(17, 1) + `},"value":1,"timestamp_ms":1}]}`, false},
		{"1024-byte label value", `{"samples":[{"metric":"m","labels":{` + specLabels(1, 1024) + `},"value":1,"timestamp_ms":1}]}`, true},
		{"1025-byte label value", `{"samples":[{"metric":"m","labels":{` + specLabels(1, 1025) + `},"value":1,"timestamp_ms":1}]}`, false},
	}
	for _, c := range cases {
		_, err := decodeSamples(strings.NewReader(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// specLabels returns n JSON label pairs whose values are valueLen bytes long.
// A copy of the helper in the root spec_test.go.
func specLabels(n, valueLen int) string {
	pairs := make([]string, n)
	for i := range pairs {
		pairs[i] = fmt.Sprintf(`"l%d":%q`, i, strings.Repeat("v", valueLen))
	}
	return strings.Join(pairs, ",")
}
