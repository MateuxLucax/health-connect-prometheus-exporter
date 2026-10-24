package remotewrite

import (
	"bytes"
	"testing"
)

func TestEncodeWriteRequest(t *testing.T) {
	got := encodeWriteRequest([]series{{
		labels: []label{{"__name__", "m"}},
		points: []point{{1, 2}},
	}})
	want := []byte{
		0x0a, 28, // timeseries (field 1), 28 bytes
		0x0a, 13, // label (field 1), 13 bytes
		0x0a, 8, '_', '_', 'n', 'a', 'm', 'e', '_', '_', // name
		0x12, 1, 'm', // value
		0x12, 11, // sample (field 2), 11 bytes
		0x09, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f, // value 1.0 as fixed64
		0x10, 2, // timestamp 2 as varint
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got  % x\nwant % x", got, want)
	}
}
