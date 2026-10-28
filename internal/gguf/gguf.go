// Package gguf reads the header metadata of a GGUF model file. It only
// parses bytes: it never loads tensors, never executes the file, and stops
// after a fixed byte budget, so a hostile or corrupt file cannot make the
// service read or allocate without bound.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Limits that bound a read. A real header (metadata plus the tokenizer
// vocabulary) is well under the byte budget.
const (
	MaxHeaderBytes = 64 << 20
	maxKVCount     = 1 << 16
	maxKeyLen      = 256
	maxStringLen   = 1 << 20
	maxKeptArray   = 4096
)

var (
	// ErrNotGGUF means the file does not start with the GGUF magic.
	ErrNotGGUF = errors.New("not a GGUF file")
	// ErrUnsupportedVersion means a GGUF version this reader does not know.
	ErrUnsupportedVersion = errors.New("unsupported GGUF version")
	// ErrTruncated means the header ends before its declared contents.
	ErrTruncated = errors.New("GGUF header is truncated")
	// ErrTooLarge means the header exceeds the read budget or declares
	// implausible counts.
	ErrTooLarge = errors.New("GGUF header is larger than the read limit")
	// ErrMalformed means a value in the header is not valid.
	ErrMalformed = errors.New("GGUF header is malformed")
)

// Metadata is the subset of a GGUF header that sizing a load needs. A zero
// number means the file did not say; callers must treat it as unknown.
type Metadata struct {
	Version     uint32 `json:"version"`
	TensorCount uint64 `json:"tensor_count"`
	Name        string `json:"name,omitempty"`
	Arch        string `json:"architecture,omitempty"`
	SizeLabel   string `json:"size_label,omitempty"`
	// FileType is general.file_type, the llama.cpp quantization enum.
	FileType uint32 `json:"file_type,omitempty"`

	ContextLength   uint64 `json:"context_length,omitempty"` // training context, tokens
	BlockCount      uint64 `json:"block_count,omitempty"`    // transformer layers
	EmbeddingLength uint64 `json:"embedding_length,omitempty"`
	HeadCount       uint64 `json:"head_count,omitempty"`
	HeadCountKV     uint64 `json:"head_count_kv,omitempty"` // largest per-layer value
	// KVHeadsTotal is the key/value head count summed over all layers, which
	// is what the KV cache scales with. Zero when it cannot be derived.
	KVHeadsTotal uint64 `json:"kv_heads_total,omitempty"`
	KeyLength    uint64 `json:"key_length,omitempty"`
	ValueLength  uint64 `json:"value_length,omitempty"`
	ExpertCount  uint64 `json:"expert_count,omitempty"`
	ExpertUsed   uint64 `json:"expert_used_count,omitempty"`
	SlidingWin   uint64 `json:"sliding_window,omitempty"`
}

const (
	tU8, tI8, tU16, tI16, tU32, tI32, tF32, tBool, tString, tArray, tU64, tI64, tF64 = 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12
)

type reader struct {
	r    *bufio.Reader
	read int64
	err  error
}

func (rd *reader) fail(err error) {
	if rd.err == nil {
		rd.err = err
	}
}

func (rd *reader) take(n int64) bool {
	if rd.err != nil {
		return false
	}
	if n < 0 || rd.read+n > MaxHeaderBytes {
		rd.fail(ErrTooLarge)
		return false
	}
	rd.read += n
	return true
}

func (rd *reader) bytes(n int) []byte {
	if !rd.take(int64(n)) {
		return nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rd.r, b); err != nil {
		rd.fail(ErrTruncated)
		return nil
	}
	return b
}

func (rd *reader) skip(n uint64) {
	if n > MaxHeaderBytes || !rd.take(int64(n)) {
		rd.fail(ErrTooLarge)
		return
	}
	if _, err := rd.r.Discard(int(n)); err != nil {
		rd.fail(ErrTruncated)
	}
}

func (rd *reader) u32() uint32 {
	b := rd.bytes(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (rd *reader) u64() uint64 {
	b := rd.bytes(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// str reads a length-prefixed string of at most limit bytes.
func (rd *reader) str(limit uint64) (string, bool) {
	n := rd.u64()
	if rd.err != nil {
		return "", false
	}
	if n > limit {
		rd.skip(n)
		return "", false
	}
	b := rd.bytes(int(n))
	return string(b), rd.err == nil
}

func scalarSize(t uint32) int {
	switch t {
	case tU8, tI8, tBool:
		return 1
	case tU16, tI16:
		return 2
	case tU32, tI32, tF32:
		return 4
	case tU64, tI64, tF64:
		return 8
	}
	return 0
}

// uint reads an integer value of type t; ok is false for non-integers and
// for negative values.
func (rd *reader) uint(t uint32) (uint64, bool) {
	b := rd.bytes(scalarSize(t))
	if b == nil {
		return 0, false
	}
	switch t {
	case tU8:
		return uint64(b[0]), true
	case tI8:
		return uint64(b[0]), int8(b[0]) >= 0
	case tU16:
		return uint64(binary.LittleEndian.Uint16(b)), true
	case tI16:
		v := int16(binary.LittleEndian.Uint16(b))
		return uint64(v), v >= 0
	case tU32:
		return uint64(binary.LittleEndian.Uint32(b)), true
	case tI32:
		v := int32(binary.LittleEndian.Uint32(b))
		return uint64(v), v >= 0
	case tU64:
		return binary.LittleEndian.Uint64(b), true
	case tI64:
		v := int64(binary.LittleEndian.Uint64(b))
		return uint64(v), v >= 0
	}
	return 0, false
}

// skipValue discards a value of type t, including nested arrays.
func (rd *reader) skipValue(t uint32, depth int) {
	switch {
	case scalarSize(t) > 0:
		rd.skip(uint64(scalarSize(t)))
	case t == tString:
		n := rd.u64()
		rd.skip(n)
	case t == tArray:
		if depth > 2 {
			rd.fail(ErrMalformed)
			return
		}
		et := rd.u32()
		n := rd.u64()
		if rd.err != nil {
			return
		}
		if sz := scalarSize(et); sz > 0 {
			if n > MaxHeaderBytes/uint64(sz) {
				rd.fail(ErrTooLarge)
				return
			}
			rd.skip(n * uint64(sz))
			return
		}
		for i := uint64(0); i < n && rd.err == nil; i++ {
			rd.skipValue(et, depth+1)
		}
	default:
		rd.fail(ErrMalformed)
	}
}

// Parse reads the GGUF header from r. It reads at most MaxHeaderBytes.
func Parse(r io.Reader) (Metadata, error) {
	rd := &reader{r: bufio.NewReaderSize(r, 64<<10)}
	var m Metadata
	magic := rd.bytes(4)
	if rd.err != nil || string(magic) != "GGUF" {
		if rd.err == ErrTruncated || rd.err == nil {
			return m, ErrNotGGUF
		}
		return m, rd.err
	}
	m.Version = rd.u32()
	if rd.err != nil {
		return m, rd.err
	}
	if m.Version < 2 || m.Version > 3 {
		return m, fmt.Errorf("%w %d", ErrUnsupportedVersion, m.Version)
	}
	m.TensorCount = rd.u64()
	kvs := rd.u64()
	if rd.err != nil {
		return m, rd.err
	}
	if kvs > maxKVCount {
		return m, ErrTooLarge
	}

	ints := map[string]uint64{}
	strs := map[string]string{}
	kvHeads := map[string]uint64{} // arch-qualified head_count_kv arrays: sum
	kvMax := map[string]uint64{}
	for i := uint64(0); i < kvs && rd.err == nil; i++ {
		key, ok := rd.str(maxKeyLen)
		t := rd.u32()
		if rd.err != nil {
			break
		}
		if !ok || !wanted(key) {
			rd.skipValue(t, 0)
			continue
		}
		switch {
		case t == tString:
			if s, ok := rd.str(maxStringLen); ok {
				strs[key] = s
			}
		case t == tArray:
			et := rd.u32()
			n := rd.u64()
			if rd.err != nil {
				break
			}
			if scalarSize(et) == 0 || n > maxKeptArray || !strings.HasSuffix(key, ".attention.head_count_kv") {
				rd.skipValue2(et, n)
				break
			}
			var sum, mx uint64
			for j := uint64(0); j < n && rd.err == nil; j++ {
				if v, ok := rd.uint(et); ok {
					sum += v
					mx = max(mx, v)
				}
			}
			kvHeads[key], kvMax[key] = sum, mx
		default:
			if scalarSize(t) == 0 {
				rd.skipValue(t, 0)
				break
			}
			if v, ok := rd.uint(t); ok {
				ints[key] = v
			}
		}
	}
	if rd.err != nil {
		return m, rd.err
	}

	m.Name, m.SizeLabel, m.Arch = strs["general.name"], strs["general.size_label"], strs["general.architecture"]
	m.FileType = uint32(min(ints["general.file_type"], math.MaxUint32))
	if m.Arch == "" {
		return m, nil
	}
	a := m.Arch + "."
	m.ContextLength, m.BlockCount, m.EmbeddingLength = ints[a+"context_length"], ints[a+"block_count"], ints[a+"embedding_length"]
	m.HeadCount, m.HeadCountKV = ints[a+"attention.head_count"], ints[a+"attention.head_count_kv"]
	m.KeyLength, m.ValueLength = ints[a+"attention.key_length"], ints[a+"attention.value_length"]
	m.ExpertCount, m.ExpertUsed = ints[a+"expert_count"], ints[a+"expert_used_count"]
	m.SlidingWin = ints[a+"attention.sliding_window"]
	if sum, ok := kvHeads[a+"attention.head_count_kv"]; ok {
		m.KVHeadsTotal, m.HeadCountKV = sum, kvMax[a+"attention.head_count_kv"]
	} else {
		if m.HeadCountKV == 0 {
			// Absent means multi-head attention: one KV head per head.
			m.HeadCountKV = m.HeadCount
		}
		m.KVHeadsTotal = m.HeadCountKV * m.BlockCount
	}
	return m, nil
}

// skipValue2 discards the n elements of an array whose header is read.
func (rd *reader) skipValue2(et uint32, n uint64) {
	if sz := scalarSize(et); sz > 0 {
		if n > MaxHeaderBytes/uint64(sz) {
			rd.fail(ErrTooLarge)
			return
		}
		rd.skip(n * uint64(sz))
		return
	}
	for i := uint64(0); i < n && rd.err == nil; i++ {
		rd.skipValue(et, 1)
	}
}

// wanted reports whether a key feeds Metadata.
func wanted(key string) bool {
	switch key {
	case "general.architecture", "general.name", "general.size_label", "general.file_type":
		return true
	}
	for _, s := range []string{".context_length", ".block_count", ".embedding_length", ".attention.head_count",
		".attention.head_count_kv", ".attention.key_length", ".attention.value_length", ".expert_count",
		".expert_used_count", ".attention.sliding_window"} {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}
