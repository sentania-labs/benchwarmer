package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type buf struct{ bytes.Buffer }

func (b *buf) u32(v uint32) { _ = binary.Write(&b.Buffer, binary.LittleEndian, v) }
func (b *buf) u64(v uint64) { _ = binary.Write(&b.Buffer, binary.LittleEndian, v) }
func (b *buf) str(s string) { b.u64(uint64(len(s))); b.WriteString(s) }

func header(version uint32, kvs uint64) *buf {
	b := &buf{}
	b.WriteString("GGUF")
	b.u32(version)
	b.u64(0)
	b.u64(kvs)
	return b
}

func (b *buf) kvStr(k, v string)        { b.str(k); b.u32(tString); b.str(v) }
func (b *buf) kvU32(k string, v uint32) { b.str(k); b.u32(tU32); b.u32(v) }

// llama31 mirrors the published Llama 3.1 8B header values.
func llama31() *buf {
	b := header(3, 11)
	b.kvStr("general.architecture", "llama")
	b.kvStr("general.name", "Llama 3.1 8B Instruct")
	b.kvU32("general.file_type", 15)
	b.kvU32("llama.context_length", 131072)
	b.kvU32("llama.block_count", 32)
	b.kvU32("llama.embedding_length", 4096)
	b.kvU32("llama.attention.head_count", 32)
	b.kvU32("llama.attention.head_count_kv", 8)
	// A tokenizer vocabulary is skipped, not kept.
	b.str("tokenizer.ggml.tokens")
	b.u32(tArray)
	b.u32(tString)
	b.u64(3)
	b.str("a")
	b.str("bb")
	b.str("ccc")
	b.str("tokenizer.ggml.scores")
	b.u32(tArray)
	b.u32(tF32)
	b.u64(3)
	b.Write(make([]byte, 12))
	b.kvStr("general.size_label", "8B")
	return b
}

func TestParseLlama(t *testing.T) {
	m, err := Parse(bytes.NewReader(llama31().Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Arch != "llama" || m.BlockCount != 32 || m.ContextLength != 131072 || m.HeadCountKV != 8 ||
		m.KVHeadsTotal != 256 || m.FileType != 15 || m.SizeLabel != "8B" || m.Name != "Llama 3.1 8B Instruct" {
		t.Fatalf("%+v", m)
	}
}

func TestParseSplitCount(t *testing.T) {
	b := header(3, 3)
	b.kvStr("general.architecture", "llama")
	b.str("split.count")
	b.u32(tU16)
	b.Write([]byte{3, 0})
	b.kvU32("llama.block_count", 32)
	m, err := Parse(bytes.NewReader(b.Bytes()))
	if err != nil || m.SplitCount != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = Parse(bytes.NewReader(llama31().Bytes()))
	if err != nil || m.SplitCount != 0 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParsePerLayerKVHeads(t *testing.T) {
	b := header(3, 4)
	b.kvStr("general.architecture", "gemma3")
	b.kvU32("gemma3.block_count", 4)
	b.str("gemma3.attention.head_count_kv")
	b.u32(tArray)
	b.u32(tU32)
	b.u64(4)
	for _, v := range []uint32{1, 1, 4, 4} {
		b.u32(v)
	}
	b.kvU32("gemma3.attention.head_count", 8)
	m, err := Parse(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if m.KVHeadsTotal != 10 || m.HeadCountKV != 4 {
		t.Fatalf("%+v", m)
	}
}

func TestParseMissingKVHeadsMeansMHA(t *testing.T) {
	b := header(2, 4)
	b.kvStr("general.architecture", "gptj")
	b.kvU32("gptj.block_count", 28)
	b.kvU32("gptj.attention.head_count", 16)
	b.kvU32("gptj.embedding_length", 4096)
	m, err := Parse(bytes.NewReader(b.Bytes()))
	if err != nil || m.KVHeadsTotal != 16*28 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParseRejects(t *testing.T) {
	good := llama31().Bytes()
	cases := map[string]struct {
		in   []byte
		want error
	}{
		"empty":     {nil, ErrNotGGUF},
		"text":      {[]byte("this is a text file, not a model"), ErrNotGGUF},
		"version":   {header(9, 0).Bytes(), ErrUnsupportedVersion},
		"v1":        {header(1, 0).Bytes(), ErrUnsupportedVersion},
		"truncated": {good[:len(good)-5], ErrTruncated},
		"huge kvs":  {header(3, 1<<40).Bytes(), ErrTooLarge},
	}
	for name, c := range cases {
		if _, err := Parse(bytes.NewReader(c.in)); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestParseBoundsHostileLengths(t *testing.T) {
	// A string claiming more bytes than the read budget.
	b := header(3, 1)
	b.str("general.name")
	b.u32(tString)
	b.u64(1 << 50)
	if _, err := Parse(bytes.NewReader(b.Bytes())); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("huge string: %v", err)
	}
	// An array claiming a billion elements.
	b = header(3, 1)
	b.str("tokenizer.ggml.tokens")
	b.u32(tArray)
	b.u32(tString)
	b.u64(1 << 30)
	if _, err := Parse(bytes.NewReader(b.Bytes())); err == nil {
		t.Fatal("endless array accepted")
	}
	// Deeply nested arrays.
	b = header(3, 1)
	b.str("x")
	b.u32(tArray)
	for i := 0; i < 6; i++ {
		b.u32(tArray)
		b.u64(1)
	}
	if _, err := Parse(bytes.NewReader(b.Bytes())); !errors.Is(err, ErrMalformed) {
		t.Fatalf("nested arrays: %v", err)
	}
}

func TestParseUnknownValueType(t *testing.T) {
	b := header(3, 1)
	b.str("x")
	b.u32(99)
	if _, err := Parse(bytes.NewReader(b.Bytes())); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
}
