package remotewrite

import "encoding/binary"

// snappyEncode encodes src in the snappy block format using only literal
// elements (valid snappy, just uncompressed).
func snappyEncode(src []byte) []byte {
	dst := binary.AppendUvarint(nil, uint64(len(src)))
	for len(src) > 0 {
		n := min(len(src), 64<<10)
		dst = appendSnappyLiteral(dst, src[:n])
		src = src[n:]
	}
	return dst
}

// appendSnappyLiteral appends a literal element. The tag's upper 6 bits hold
// len-1 when it is below 60; otherwise 60..63 say that len-1 follows in
// 1..4 little-endian bytes.
func appendSnappyLiteral(dst, lit []byte) []byte {
	n := uint32(len(lit) - 1)
	switch {
	case n < 60:
		dst = append(dst, byte(n)<<2)
	case n < 1<<8:
		dst = append(dst, 60<<2, byte(n))
	case n < 1<<16:
		dst = append(dst, 61<<2, byte(n), byte(n>>8))
	case n < 1<<24:
		dst = append(dst, 62<<2, byte(n), byte(n>>8), byte(n>>16))
	default:
		dst = append(dst, 63<<2, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	}
	return append(dst, lit...)
}
