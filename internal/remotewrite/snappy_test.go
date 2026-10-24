package remotewrite

import (
	"bytes"
	"testing"
)

func TestSnappyLiteralTags(t *testing.T) {
	// Tag encodings from the snappy format description, section 2.1.
	cases := []struct {
		n      int
		header []byte
	}{
		{1, []byte{0x00}},
		{60, []byte{59 << 2}},
		{61, []byte{60 << 2, 60}},
		{256, []byte{60 << 2, 0xff}},
		{257, []byte{61 << 2, 0x00, 0x01}},
		{1 << 16, []byte{61 << 2, 0xff, 0xff}},
		{1<<16 + 1, []byte{62 << 2, 0x00, 0x00, 0x01}},
		{1<<24 + 1, []byte{63 << 2, 0x00, 0x00, 0x00, 0x01}},
	}
	for _, c := range cases {
		lit := bytes.Repeat([]byte{'x'}, c.n)
		got := appendSnappyLiteral(nil, lit)
		if !bytes.Equal(got[:len(c.header)], c.header) || !bytes.Equal(got[len(c.header):], lit) {
			t.Errorf("len %d: header % x, want % x", c.n, got[:len(c.header)], c.header)
		}
	}
}

func TestSnappyEncode(t *testing.T) {
	if got := snappyEncode(nil); !bytes.Equal(got, []byte{0x00}) {
		t.Errorf("empty: % x", got)
	}
	if got, want := snappyEncode([]byte("abc")), []byte{0x03, 0x08, 'a', 'b', 'c'}; !bytes.Equal(got, want) {
		t.Errorf("abc: % x, want % x", got, want)
	}
	// 64 KiB + 1 bytes: uvarint length, then a 64 KiB literal and a 1-byte literal.
	src := bytes.Repeat([]byte{'y'}, 1<<16+1)
	got := snappyEncode(src)
	var want []byte
	want = append(want, 0x81, 0x80, 0x04)
	want = append(want, 61<<2, 0xff, 0xff)
	want = append(want, src[:1<<16]...)
	want = append(want, 0x00, 'y')
	if !bytes.Equal(got, want) {
		t.Errorf("64KiB+1: mismatch (len %d, want %d)", len(got), len(want))
	}
}
