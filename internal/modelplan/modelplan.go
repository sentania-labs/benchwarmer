// Package modelplan turns a model's GGUF metadata and the GPU's memory into
// proposed runtime settings. Every number it returns is an estimate from file
// metadata, not an observation of a loaded model, and each proposal says so.
// It proposes nothing it cannot back with a stated reason, and it never
// claims a model fits.
package modelplan

import (
	"fmt"

	"github.com/sentania-labs/benchwarmer/internal/gguf"
)

const (
	mib = 1 << 20
	// OverheadMiB is allowed for compute buffers and the CUDA/Vulkan context
	// on top of weights and KV cache. A fixed allowance: real overhead
	// varies with batch size and backend.
	OverheadMiB = 768
	// UsableFraction of total VRAM the estimate may fill, leaving room for
	// the desktop and driver.
	UsableFraction = 0.90
	// kvBytesPerElem is the default f16 KV cache element size. A runtime
	// started with a quantized cache (-ctk/-ctv) uses less.
	kvBytesPerElem = 2
	ctxStep        = 1024
	vramStep       = 256
)

// Certainty says where a proposed value comes from.
const (
	FromFile = "from_file"
	Estimate = "estimate"
)

// GPU is what is known about the GPU. TotalMiB zero means unknown.
type GPU struct {
	TotalMiB int
	// Reliable is false when telemetry is absent, so the total cannot be
	// trusted.
	Reliable bool
}

// Input is everything a plan is computed from.
type Input struct {
	Meta     gguf.Metadata
	FileSize int64
	GPU      GPU
	// CurrentContext is the context size already in the draft, 0 if unset.
	CurrentContext int
}

// Recommendation proposes one config field value.
type Recommendation struct {
	Path      string `json:"path"`
	Value     int    `json:"value"`
	Reason    string `json:"reason"`
	Certainty string `json:"certainty"`
}

// Plan is the result. Unset numbers are zero and mean unknown.
type Plan struct {
	WeightsMiB    int `json:"weights_mib"`
	KVBytesPerTok int `json:"kv_bytes_per_token,omitempty"`
	OverheadMiB   int `json:"overhead_mib"`
	// EstimatedMiB is weights, KV cache at the proposed (or current)
	// context, and overhead.
	EstimatedMiB    int              `json:"estimated_mib,omitempty"`
	Recommendations []Recommendation `json:"recommendations"`
	// Notes are the uncertainties and limits of this estimate.
	Notes []string `json:"notes"`
	// Fit is "unknown", "likely_fits" or "may_not_fit". It is never a
	// guarantee.
	Fit string `json:"fit"`
}

// KVBytesPerToken is the f16 KV cache cost of one context token, or 0 when
// the file does not say enough to compute it.
func KVBytesPerToken(m gguf.Metadata) int {
	k, v := m.KeyLength, m.ValueLength
	if k == 0 && m.HeadCount > 0 {
		k = m.EmbeddingLength / m.HeadCount
	}
	if v == 0 {
		v = k
	}
	if m.KVHeadsTotal == 0 || k == 0 || v == 0 {
		return 0
	}
	return int(m.KVHeadsTotal * (k + v) * kvBytesPerElem)
}

func roundUp(v, step int) int   { return (v + step - 1) / step * step }
func roundDown(v, step int) int { return v / step * step }

// Compute builds the plan for one model.
func Compute(in Input) Plan {
	p := Plan{OverheadMiB: OverheadMiB, Recommendations: []Recommendation{}, Notes: []string{}, Fit: "unknown"}
	m := in.Meta
	p.WeightsMiB = int((in.FileSize + mib - 1) / mib)
	p.KVBytesPerTok = KVBytesPerToken(m)

	if m.ExpertCount > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("This is a mixture-of-experts model (%d experts). The file size counts every expert; moving expert layers to the CPU (--n-cpu-moe) lowers VRAM use but is not changed for you.", m.ExpertCount))
	}
	if m.SlidingWin > 0 {
		p.Notes = append(p.Notes, "This model uses sliding-window attention, so the real KV cache can be smaller than estimated here.")
	}
	p.Notes = append(p.Notes, "Memory figures assume an f16 KV cache and a fixed allowance for compute buffers. They are estimates; the loaded model may use more or less.")

	ctx := in.CurrentContext
	if m.ContextLength > 0 && ctx > int(m.ContextLength) {
		ctx = int(m.ContextLength)
		p.Recommendations = append(p.Recommendations, Recommendation{
			Path: "runtime.context_size", Value: ctx, Certainty: FromFile,
			Reason: fmt.Sprintf("the model was trained for %d tokens of context; %d is more than it supports", m.ContextLength, in.CurrentContext),
		})
	}

	if !in.GPU.Reliable || in.GPU.TotalMiB <= 0 {
		p.Notes = append(p.Notes, "GPU memory is not known right now, so no memory-based settings are proposed.")
		return p
	}
	if p.KVBytesPerTok == 0 || p.WeightsMiB == 0 {
		p.Notes = append(p.Notes, "The file does not give enough detail to estimate the KV cache, so no memory-based settings are proposed.")
		return p
	}

	budget := int(float64(in.GPU.TotalMiB) * UsableFraction)
	room := budget - p.WeightsMiB - OverheadMiB
	if room <= 0 {
		p.Fit = "may_not_fit"
		p.Notes = append(p.Notes, fmt.Sprintf("The weights alone (about %d MiB) plus overhead leave no room in %d MiB of GPU memory at 90%% use. The model may still run with layers or experts on the CPU; nothing was changed.", p.WeightsMiB, in.GPU.TotalMiB))
		return p
	}
	kvMiB := func(tokens int) int { return int((int64(tokens)*int64(p.KVBytesPerTok) + mib - 1) / mib) }
	fitTokens := int(int64(room) * mib / int64(p.KVBytesPerTok))
	if m.ContextLength > 0 {
		fitTokens = min(fitTokens, int(m.ContextLength))
	}
	fitTokens = roundDown(fitTokens, ctxStep)
	if fitTokens < ctxStep {
		p.Fit = "may_not_fit"
		p.Notes = append(p.Notes, "Less than 1024 tokens of context fit alongside the weights in the estimate; nothing was changed.")
		return p
	}

	// Only lower the context: a context the owner already chose smaller is
	// theirs to keep.
	if ctx == 0 || ctx > fitTokens {
		prev := ctx
		ctx = fitTokens
		p.Recommendations = dropPath(p.Recommendations, "runtime.context_size")
		reason := fmt.Sprintf("about %d MiB of weights plus %d MiB of KV cache at %d tokens fits in %d MiB of GPU memory at 90%% use", p.WeightsMiB, kvMiB(ctx), ctx, in.GPU.TotalMiB)
		if prev > 0 {
			reason = fmt.Sprintf("%d tokens would need about %d MiB of KV cache; ", prev, kvMiB(prev)) + reason
		}
		p.Recommendations = append(p.Recommendations, Recommendation{Path: "runtime.context_size", Value: ctx, Certainty: Estimate, Reason: reason})
	}
	if ctx == 0 {
		return p
	}

	need := p.WeightsMiB + kvMiB(ctx) + OverheadMiB
	p.EstimatedMiB = need
	free := roundUp(need, vramStep)
	if free > in.GPU.TotalMiB {
		free = in.GPU.TotalMiB
	}
	p.Fit = "likely_fits"
	p.Recommendations = append(p.Recommendations, Recommendation{
		Path: "runtime.required_free_vram_mib", Value: free, Certainty: Estimate,
		Reason: fmt.Sprintf("estimated load of %d MiB (weights %d, KV cache %d, overhead %d), rounded up to %d MiB", need, p.WeightsMiB, kvMiB(ctx), OverheadMiB, vramStep),
	})
	return p
}

func dropPath(rs []Recommendation, path string) []Recommendation {
	out := rs[:0]
	for _, r := range rs {
		if r.Path != path {
			out = append(out, r)
		}
	}
	return out
}
