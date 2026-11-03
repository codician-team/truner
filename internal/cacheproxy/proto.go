package cacheproxy

import (
	"encoding/binary"
	"errors"
)

// A tiny protobuf wire-format codec for the handful of CacheService messages,
// so clients using application/protobuf twirp work too.

type pbField struct {
	varint uint64
	bytes  []byte
}

func pbDecode(b []byte) (map[int][]pbField, error) {
	out := map[int][]pbField{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("protobuf: bad tag")
		}
		b = b[n:]
		num, wt := int(tag>>3), tag&7
		switch wt {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("protobuf: bad varint")
			}
			b = b[n:]
			out[num] = append(out[num], pbField{varint: v})
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("protobuf: bad length")
			}
			b = b[n:]
			out[num] = append(out[num], pbField{bytes: b[:l]})
			b = b[l:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("protobuf: short fixed64")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("protobuf: short fixed32")
			}
			b = b[4:]
		default:
			return nil, errors.New("protobuf: unsupported wire type")
		}
	}
	return out, nil
}

func pbString(m map[int][]pbField, n int) string {
	if f := m[n]; len(f) > 0 {
		return string(f[len(f)-1].bytes)
	}
	return ""
}

func pbStrings(m map[int][]pbField, n int) []string {
	var out []string
	for _, f := range m[n] {
		out = append(out, string(f.bytes))
	}
	return out
}

func pbInt(m map[int][]pbField, n int) int64 {
	if f := m[n]; len(f) > 0 {
		return int64(f[len(f)-1].varint)
	}
	return 0
}

type pbWriter struct{ b []byte }

func (w *pbWriter) tag(n int, wt uint64) { w.b = binary.AppendUvarint(w.b, uint64(n)<<3|wt) }

func (w *pbWriter) str(n int, s string) {
	if s == "" {
		return
	}
	w.tag(n, 2)
	w.b = binary.AppendUvarint(w.b, uint64(len(s)))
	w.b = append(w.b, s...)
}

func (w *pbWriter) boolean(n int, v bool) {
	if !v {
		return
	}
	w.tag(n, 0)
	w.b = append(w.b, 1)
}

func (w *pbWriter) int64(n int, v int64) {
	if v == 0 {
		return
	}
	w.tag(n, 0)
	w.b = binary.AppendUvarint(w.b, uint64(v))
}
