package ingest

// A copy of the snappy and protobuf decoders in the root spec_test.go, written
// from the public format descriptions and independent of the production
// encoder. Test helpers are copied per package instead of shared (ADR 0012).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ---------------------------------------------------------------------------
// Snappy block format decoder (literals and all three copy element kinds).
// https://github.com/google/snappy/blob/main/format_description.txt

func specSnappyDecode(src []byte) ([]byte, error) {
	want, n := binary.Uvarint(src)
	if n <= 0 {
		return nil, errors.New("snappy: bad length preamble")
	}
	if want > 1<<30 {
		return nil, errors.New("snappy: length too large")
	}
	src = src[n:]
	dst := make([]byte, 0, want)
	for len(src) > 0 {
		tag := src[0]
		src = src[1:]
		switch tag & 3 {
		case 0: // literal
			l := uint64(tag >> 2)
			if l >= 60 {
				nb := int(l - 59)
				if len(src) < nb {
					return nil, errors.New("snappy: short literal length")
				}
				l = 0
				for i := 0; i < nb; i++ {
					l |= uint64(src[i]) << (8 * i)
				}
				src = src[nb:]
			}
			l++
			if uint64(len(src)) < l {
				return nil, errors.New("snappy: short literal")
			}
			dst = append(dst, src[:l]...)
			src = src[l:]
		default:
			var length, offset int
			switch tag & 3 {
			case 1:
				if len(src) < 1 {
					return nil, errors.New("snappy: short copy1")
				}
				length = 4 + int(tag>>2&7)
				offset = int(tag>>5)<<8 | int(src[0])
				src = src[1:]
			case 2:
				if len(src) < 2 {
					return nil, errors.New("snappy: short copy2")
				}
				length = 1 + int(tag>>2)
				offset = int(src[0]) | int(src[1])<<8
				src = src[2:]
			case 3:
				if len(src) < 4 {
					return nil, errors.New("snappy: short copy4")
				}
				length = 1 + int(tag>>2)
				offset = int(binary.LittleEndian.Uint32(src))
				src = src[4:]
			}
			if offset == 0 || offset > len(dst) {
				return nil, fmt.Errorf("snappy: bad copy offset %d (have %d)", offset, len(dst))
			}
			start := len(dst) - offset
			for i := 0; i < length; i++ { // byte by byte: copies may overlap
				dst = append(dst, dst[start+i])
			}
		}
	}
	if uint64(len(dst)) != want {
		return nil, fmt.Errorf("snappy: decoded %d bytes, preamble says %d", len(dst), want)
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// Protobuf wire format decoder for prometheus.WriteRequest.

type specLabel struct{ Name, Value string }

type specSample struct {
	Value float64
	Ts    int64
}

type specSeries struct {
	Labels  []specLabel
	Samples []specSample
}

type pbField struct {
	num  uint64
	wire uint64
	v    uint64 // varint / fixed64 / fixed32
	b    []byte // length-delimited
}

func pbFields(b []byte) ([]pbField, error) {
	var out []pbField
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("pb: bad key")
		}
		b = b[n:]
		f := pbField{num: key >> 3, wire: key & 7}
		switch f.wire {
		case 0:
			f.v, n = binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("pb: bad varint")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("pb: short fixed64")
			}
			f.v = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("pb: bad length-delimited field")
			}
			f.b = b[n : n+int(l)]
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("pb: short fixed32")
			}
			f.v = uint64(binary.LittleEndian.Uint32(b))
			b = b[4:]
		default:
			return nil, fmt.Errorf("pb: unsupported wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}

func specDecodeWriteRequest(b []byte) ([]specSeries, error) {
	top, err := pbFields(b)
	if err != nil {
		return nil, err
	}
	var out []specSeries
	for _, tf := range top {
		if tf.num != 1 {
			continue
		}
		if tf.wire != 2 {
			return nil, errors.New("pb: timeseries not length-delimited")
		}
		fs, err := pbFields(tf.b)
		if err != nil {
			return nil, err
		}
		var s specSeries
		for _, f := range fs {
			switch f.num {
			case 1: // Label
				if f.wire != 2 {
					return nil, errors.New("pb: label wire type")
				}
				lfs, err := pbFields(f.b)
				if err != nil {
					return nil, err
				}
				var l specLabel
				for _, lf := range lfs {
					switch {
					case lf.num == 1 && lf.wire == 2:
						l.Name = string(lf.b)
					case lf.num == 2 && lf.wire == 2:
						l.Value = string(lf.b)
					}
				}
				s.Labels = append(s.Labels, l)
			case 2: // Sample
				if f.wire != 2 {
					return nil, errors.New("pb: sample wire type")
				}
				sfs, err := pbFields(f.b)
				if err != nil {
					return nil, err
				}
				var sm specSample
				for _, sf := range sfs {
					switch {
					case sf.num == 1 && sf.wire == 1:
						sm.Value = math.Float64frombits(sf.v)
					case sf.num == 2 && sf.wire == 0:
						sm.Ts = int64(sf.v)
					}
				}
				s.Samples = append(s.Samples, sm)
			}
		}
		out = append(out, s)
	}
	return out, nil
}
