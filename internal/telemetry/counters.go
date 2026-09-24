package telemetry

import (
	"strconv"
	"strings"
)

// RawCounters is one collection of the Windows GPU performance counter sets,
// keyed by counter instance name. It is platform-neutral so the parsing and
// attribution logic can be tested anywhere.
type RawCounters struct {
	EngineUtil       map[string]float64 // \GPU Engine(*)\Utilization Percentage
	ProcDedicated    map[string]float64 // \GPU Process Memory(*)\Dedicated Usage
	ProcShared       map[string]float64 // \GPU Process Memory(*)\Shared Usage
	AdapterDedicated map[string]float64 // \GPU Adapter Memory(*)\Dedicated Usage
	AdapterShared    map[string]float64 // \GPU Adapter Memory(*)\Shared Usage
}

// instance is a parsed GPU counter instance name, for example
// "pid_1234_luid_0x00000000_0x0000C3F1_phys_0_eng_3_engtype_Compute".
type instance struct {
	PID     uint32
	HasPID  bool
	LUID    string // "0x00000000_0x0000C3F1"
	Phys    string
	Eng     string
	EngType string
}

func parseInstance(s string) (instance, bool) {
	var in instance
	rest := s
	if v, ok := strings.CutPrefix(rest, "pid_"); ok {
		i := strings.IndexByte(v, '_')
		if i < 0 {
			return in, false
		}
		n, err := strconv.ParseUint(v[:i], 10, 32)
		if err != nil {
			return in, false
		}
		in.PID, in.HasPID = uint32(n), true
		rest = v[i+1:]
	}
	v, ok := strings.CutPrefix(rest, "luid_")
	if !ok || len(v) < 21 {
		return in, false
	}
	in.LUID = strings.ToLower(v[:21])
	rest = v[21:]
	if v, ok := strings.CutPrefix(rest, "_phys_"); ok {
		i := strings.IndexByte(v, '_')
		if i < 0 {
			in.Phys = v
			return in, true
		}
		in.Phys, rest = v[:i], v[i:]
	}
	if v, ok := strings.CutPrefix(rest, "_eng_"); ok {
		i := strings.IndexByte(v, '_')
		if i < 0 {
			in.Eng = v
			return in, true
		}
		in.Eng, rest = v[:i], v[i:]
	}
	if v, ok := strings.CutPrefix(rest, "_engtype_"); ok {
		in.EngType = v
	}
	return in, true
}

// EngineClass maps a driver-reported engine type onto a small set of classes.
// AMD drivers report names such as "Compute_0" or "High Priority Compute".
func EngineClass(raw string) string {
	l := strings.ToLower(raw)
	switch {
	case strings.Contains(l, "3d") || strings.Contains(l, "graphics"):
		return "3D"
	case strings.Contains(l, "compute"):
		return "Compute"
	case strings.Contains(l, "copy") || strings.Contains(l, "dma"):
		return "Copy"
	case strings.Contains(l, "video") || strings.Contains(l, "codec") || strings.Contains(l, "vcn"):
		return "Video"
	case raw == "":
		return "Unknown"
	default:
		return "Other"
	}
}

// FormatLUID renders a LUID the way GPU counter instance names do.
func FormatLUID(high int32, low uint32) string {
	return "0x" + hex8(uint32(high)) + "_0x" + hex8(low)
}

func hex8(v uint32) string {
	s := strconv.FormatUint(uint64(v), 16)
	return strings.Repeat("0", 8-len(s)) + s
}

// FromCounters builds the counter-derived part of a Sample for one adapter.
// An empty luid selects the adapter with the most dedicated memory in use,
// which is how the probe behaves before an adapter is configured.
func FromCounters(rc RawCounters, luid string) Sample {
	luid = strings.ToLower(luid)
	if luid == "" {
		luid = busiestLUID(rc.AdapterDedicated)
	}
	s := Sample{AdapterLUID: luid}
	for name, v := range rc.AdapterDedicated {
		if in, ok := parseInstance(name); ok && in.LUID == luid {
			s.DedicatedUsedBytes += uint64(v)
			s.HasAdapterInstances = true
		}
	}
	for name, v := range rc.AdapterShared {
		if in, ok := parseInstance(name); ok && in.LUID == luid {
			s.SharedUsedBytes += uint64(v)
		}
	}
	procs := map[uint32]*ProcGPU{}
	proc := func(pid uint32) *ProcGPU {
		p := procs[pid]
		if p == nil {
			p = &ProcGPU{PID: pid, EngineUtil: map[string]float64{}, EngineByKey: map[string]float64{}}
			procs[pid] = p
		}
		return p
	}
	engines := map[string]*Engine{}
	for name, v := range rc.EngineUtil {
		in, ok := parseInstance(name)
		if !ok || in.LUID != luid {
			continue
		}
		key := "phys_" + in.Phys + "_eng_" + in.Eng
		class := EngineClass(in.EngType)
		e := engines[key]
		if e == nil {
			e = &Engine{Key: key, Type: class}
			engines[key] = e
		}
		e.UtilPct += v
		if in.HasPID {
			p := proc(in.PID)
			p.EngineByKey[key] += v
		}
	}
	for name, v := range rc.ProcDedicated {
		if in, ok := parseInstance(name); ok && in.LUID == luid && in.HasPID {
			proc(in.PID).DedicatedBytes += uint64(v)
		}
	}
	for name, v := range rc.ProcShared {
		if in, ok := parseInstance(name); ok && in.LUID == luid && in.HasPID {
			proc(in.PID).SharedBytes += uint64(v)
		}
	}
	for _, e := range engines {
		e.UtilPct = clampPct(e.UtilPct)
		s.Engines = append(s.Engines, *e)
	}
	for _, p := range procs {
		for k, v := range p.EngineByKey {
			c := engines[k].Type
			p.EngineUtil[c] = max(p.EngineUtil[c], v)
		}
		s.Processes = append(s.Processes, *p)
	}
	return s
}

func busiestLUID(m map[string]float64) string {
	best, bestV := "", -1.0
	per := map[string]float64{}
	for name, v := range m {
		if in, ok := parseInstance(name); ok {
			per[in.LUID] += v
		}
	}
	for l, v := range per {
		if v > bestV || (v == bestV && l < best) {
			best, bestV = l, v
		}
	}
	return best
}
