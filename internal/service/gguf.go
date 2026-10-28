package service

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// ggufArchitecture reads only bounded metadata. Files that are malformed,
// newer than the supported format, or too large to inspect remain unknown.
func ggufArchitecture(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := &ggufReader{r: io.LimitReader(f, 16<<20)}
	var magic [4]byte
	if _, err = io.ReadFull(r.r, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return ""
	}
	version := r.u32()
	_ = r.u64()
	count := r.u64()
	if version < 2 || version > 3 || count > 100000 {
		return ""
	}
	for i := uint64(0); i < count && r.err == nil; i++ {
		key := r.str()
		typ := r.u32()
		if key == "general.architecture" && typ == 8 {
			value := r.str()
			if r.err == nil {
				return value
			}
			return ""
		}
		r.skip(typ, false)
	}
	return ""
}

type ggufReader struct {
	r   io.Reader
	err error
}

func (r *ggufReader) read(v any) {
	if r.err == nil {
		r.err = binary.Read(r.r, binary.LittleEndian, v)
	}
}
func (r *ggufReader) u32() uint32 { var v uint32; r.read(&v); return v }
func (r *ggufReader) u64() uint64 { var v uint64; r.read(&v); return v }
func (r *ggufReader) str() string {
	n := r.u64()
	if n > 1<<20 {
		r.err = errors.New("oversized GGUF string")
		return ""
	}
	b := make([]byte, int(n))
	if r.err == nil {
		_, r.err = io.ReadFull(r.r, b)
	}
	return string(b)
}
func (r *ggufReader) skip(t uint32, nested bool) {
	sizes := map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}
	if n, ok := sizes[t]; ok {
		if r.err == nil {
			_, r.err = io.CopyN(io.Discard, r.r, n)
		}
		return
	}
	if t == 8 {
		_ = r.str()
		return
	}
	if t == 9 && !nested {
		elem := r.u32()
		n := r.u64()
		if n > 1000000 {
			r.err = errors.New("oversized GGUF array")
			return
		}
		for i := uint64(0); i < n && r.err == nil; i++ {
			r.skip(elem, true)
		}
		return
	}
	r.err = errors.New("invalid GGUF type")
}
