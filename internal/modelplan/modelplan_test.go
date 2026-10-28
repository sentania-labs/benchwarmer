package modelplan

import (
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/gguf"
)

// llama31 8B: 32 layers x 8 KV heads x (128 key + 128 value) x 2 bytes
// = 131072 bytes (128 KiB) per token, the published figure.
func llama31() gguf.Metadata {
	return gguf.Metadata{Arch: "llama", ContextLength: 131072, BlockCount: 32, EmbeddingLength: 4096,
		HeadCount: 32, HeadCountKV: 8, KVHeadsTotal: 256}
}

func rec(p Plan, path string) *Recommendation {
	for i := range p.Recommendations {
		if p.Recommendations[i].Path == path {
			return &p.Recommendations[i]
		}
	}
	return nil
}

func TestKVBytesPerToken(t *testing.T) {
	if got := KVBytesPerToken(llama31()); got != 131072 {
		t.Fatalf("kv bytes/token = %d, want 131072", got)
	}
	m := llama31()
	m.KeyLength, m.ValueLength = 192, 128 // asymmetric head sizes
	if got := KVBytesPerToken(m); got != 256*(192+128)*2 {
		t.Fatalf("asymmetric = %d", got)
	}
	if KVBytesPerToken(gguf.Metadata{BlockCount: 32}) != 0 {
		t.Fatal("unknown shape must be 0")
	}
}

// 8B Q4_K_M is about 4.58 GiB. On a 24 GiB card: budget 22118, room
// 22118-4690-768 = 16660 MiB, which is 133280 tokens, capped to the trained
// 131072; so the trained context fits and the 262144 draft is lowered to it.
func TestComputeCapsAtTrainedContextWhenItFits(t *testing.T) {
	p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 262144})
	r := rec(p, "runtime.context_size")
	if r == nil || r.Value != 131072 || r.Certainty != FromFile {
		t.Fatalf("%+v", p.Recommendations)
	}
	free := rec(p, "runtime.required_free_vram_mib")
	// 4690 + 16384 (131072 tokens x 128 KiB) + 768 = 21842 -> 22016
	if free == nil || free.Value != 22016 || p.EstimatedMiB != 21842 || p.Fit != "likely_fits" {
		t.Fatalf("%+v est %d fit %s", p.Recommendations, p.EstimatedMiB, p.Fit)
	}
}

func TestComputeLowersContextToFit(t *testing.T) {
	// 16 GiB card: room = 14745-4690-768 = 9287 MiB = 74296 tokens -> 73728.
	p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, GPU: GPU{TotalMiB: 16384, Reliable: true}, CurrentContext: 131072})
	r := rec(p, "runtime.context_size")
	if r == nil || r.Value != 73728 || r.Certainty != Estimate || !strings.Contains(r.Reason, "131072 tokens") {
		t.Fatalf("%+v", p.Recommendations)
	}
}

func TestComputeNeverRaisesContext(t *testing.T) {
	p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 8192})
	if rec(p, "runtime.context_size") != nil {
		t.Fatalf("raised an owner-chosen context: %+v", p.Recommendations)
	}
	// 4690 + 1024 + 768 = 6482 -> 6656
	if r := rec(p, "runtime.required_free_vram_mib"); r == nil || r.Value != 6656 {
		t.Fatalf("%+v", p.Recommendations)
	}
}

func TestComputeUnsetContextGetsAProposal(t *testing.T) {
	p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}})
	if r := rec(p, "runtime.context_size"); r == nil || r.Value != 131072 {
		t.Fatalf("%+v", p.Recommendations)
	}
}

func TestComputeWeightsDoNotFit(t *testing.T) {
	p := Compute(Input{Meta: llama31(), FileSize: 30000 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 4096})
	if len(p.Recommendations) != 0 || p.Fit != "may_not_fit" {
		t.Fatalf("%+v fit %s", p.Recommendations, p.Fit)
	}
}

func TestComputeUnknownGPU(t *testing.T) {
	for _, g := range []GPU{{}, {TotalMiB: 24576}, {Reliable: true}} {
		p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, GPU: g, CurrentContext: 8192})
		if len(p.Recommendations) != 0 || p.Fit != "unknown" || len(p.Notes) == 0 {
			t.Fatalf("gpu %+v: %+v", g, p)
		}
	}
	// A trained-context cap needs no GPU: it comes from the file.
	p := Compute(Input{Meta: llama31(), FileSize: 4690 << 20, CurrentContext: 262144})
	if r := rec(p, "runtime.context_size"); r == nil || r.Value != 131072 || p.Fit != "unknown" {
		t.Fatalf("%+v", p)
	}
}

func TestComputeUnknownShape(t *testing.T) {
	m := gguf.Metadata{Arch: "mystery"}
	p := Compute(Input{Meta: m, FileSize: 4690 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 8192})
	if len(p.Recommendations) != 0 || p.Fit != "unknown" {
		t.Fatalf("%+v", p)
	}
}

func TestComputeNotesMoEAndSlidingWindow(t *testing.T) {
	m := llama31()
	m.ExpertCount, m.SlidingWin = 128, 4096
	p := Compute(Input{Meta: m, FileSize: 4690 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 8192})
	joined := strings.Join(p.Notes, " ")
	if !strings.Contains(joined, "mixture-of-experts") || !strings.Contains(joined, "sliding-window") {
		t.Fatal(joined)
	}
}

func TestRequiredFreeCappedAtTotal(t *testing.T) {
	m := llama31()
	p := Compute(Input{Meta: m, FileSize: 20000 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 4096})
	if r := rec(p, "runtime.required_free_vram_mib"); r == nil || r.Value > 24576 {
		t.Fatalf("%+v", p.Recommendations)
	}
}

func TestComputeSplitModelProposesNothingFromMemory(t *testing.T) {
	m := llama31()
	m.SplitCount = 3
	p := Compute(Input{Meta: m, FileSize: 1600 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 4096})
	if len(p.Recommendations) != 0 || p.Fit != "unknown" || p.EstimatedMiB != 0 {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(strings.Join(p.Notes, " "), "split across 3 files") {
		t.Fatalf("%v", p.Notes)
	}
}

func TestComputeContextNeverExceedsConfigMax(t *testing.T) {
	huge := gguf.Metadata{Arch: "tiny", BlockCount: 1, EmbeddingLength: 64, HeadCount: 1, HeadCountKV: 1, KVHeadsTotal: 1}
	p := Compute(Input{Meta: huge, FileSize: 100 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}})
	if r := rec(p, "runtime.context_size"); r == nil || r.Value != maxContext {
		t.Fatalf("%+v", p.Recommendations)
	}
	m := huge
	m.ContextLength = 4 << 20
	p = Compute(Input{Meta: m, FileSize: 100 << 20, GPU: GPU{TotalMiB: 24576, Reliable: true}, CurrentContext: 3 << 20})
	for _, r := range p.Recommendations {
		if r.Path == "runtime.context_size" && r.Value > maxContext {
			t.Fatalf("%+v", p.Recommendations)
		}
	}
}
