package telemetry

import (
	"encoding/json"
	"math"
	"testing"
)

const (
	gpu  = "0x00000000_0x0000c3f1"
	igpu = "0x00000000_0x0000aa01"
)

func eng(pid int, luid, e, t string) string {
	return "pid_" + itoa(pid) + "_luid_" + luid + "_phys_0_eng_" + e + "_engtype_" + t
}
func pmem(pid int, luid string) string { return "pid_" + itoa(pid) + "_luid_" + luid + "_phys_0" }
func amem(luid string) string          { return "luid_" + luid + "_phys_0" }

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

const gib = 1 << 30

func TestParseInstance(t *testing.T) {
	in, ok := parseInstance("pid_4242_luid_0x00000000_0x0000C3F1_phys_0_eng_12_engtype_High Priority Compute")
	if !ok || in.PID != 4242 || in.LUID != gpu || in.Phys != "0" || in.Eng != "12" || in.EngType != "High Priority Compute" {
		t.Fatalf("got %+v ok=%v", in, ok)
	}
	in, ok = parseInstance("luid_0x00000000_0x0000C3F1_phys_0")
	if !ok || in.HasPID || in.LUID != gpu {
		t.Fatalf("adapter instance: %+v", in)
	}
	if _, ok := parseInstance("_Total"); ok {
		t.Fatal("_Total should not parse")
	}
}

func TestEngineClass(t *testing.T) {
	for raw, want := range map[string]string{
		"3D": "3D", "High Priority 3D": "3D", "Compute_0": "Compute", "High Priority Compute": "Compute",
		"Copy": "Copy", "VideoDecode": "Video", "Video Codec_0": "Video", "Timer_0": "Other", "": "Unknown",
	} {
		if got := EngineClass(raw); got != want {
			t.Errorf("EngineClass(%q)=%q want %q", raw, got, want)
		}
	}
}

func fixture() RawCounters {
	const llama, game, dwm = 100, 200, 300
	return RawCounters{
		EngineUtil: map[string]float64{
			eng(llama, gpu, "1", "Compute_0"): 95,
			eng(game, gpu, "0", "3D"):         40,
			eng(dwm, gpu, "0", "3D"):          5,
			eng(game, gpu, "2", "Copy"):       10,
			eng(dwm, igpu, "0", "3D"):         70, // other adapter, ignored
		},
		ProcDedicated: map[string]float64{
			pmem(llama, gpu): 12 * gib,
			pmem(game, gpu):  2 * gib,
			pmem(dwm, gpu):   0.25 * gib,
		},
		AdapterDedicated: map[string]float64{amem(gpu): 14.5 * gib, amem(igpu): 0.1 * gib},
	}
}

func TestSplitSeparatesOwnFromExternal(t *testing.T) {
	s := FromCounters(fixture(), "")
	if s.AdapterLUID != gpu {
		t.Fatalf("auto-selected %s", s.AdapterLUID)
	}
	s.DedicatedTotalBytes = 16 * gib
	d := Split(s, map[uint32]bool{100: true})

	if !d.Attributed {
		t.Fatal("expected attributed")
	}
	if d.OwnUtilPct != 95 || d.ExternalUtilPct != 45 || d.TotalUtilPct != 95 {
		t.Fatalf("util own=%v ext=%v total=%v", d.OwnUtilPct, d.ExternalUtilPct, d.TotalUtilPct)
	}
	if d.ExternalUtilByType["3D"] != 45 || d.ExternalUtilByType["Compute"] != 0 {
		t.Fatalf("by type %v", d.ExternalUtilByType)
	}
	if d.OwnDedicatedBytes != 12*gib {
		t.Fatalf("own mem %d", d.OwnDedicatedBytes)
	}
	// Adapter total minus own (2.5 GiB) exceeds the per-process external sum
	// (2.25 GiB), so the adapter-derived figure wins.
	if math.Abs(float64(d.ExternalDedicatedBytes)-2.5*gib) > 1 {
		t.Fatalf("external mem %v GiB", float64(d.ExternalDedicatedBytes)/gib)
	}
	if d.FreeDedicatedBytes != 1.5*gib {
		t.Fatalf("free %v", d.FreeDedicatedBytes)
	}
	if len(d.TopExternal) == 0 || d.TopExternal[0].PID != 200 {
		t.Fatalf("top external %+v", d.TopExternal)
	}
}

func TestSplitSurvivesJSONRoundTrip(t *testing.T) {
	s := FromCounters(fixture(), gpu)
	b, _ := json.Marshal(s)
	var back Sample
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	a, c := Split(s, map[uint32]bool{100: true}), Split(back, map[uint32]bool{100: true})
	if a.ExternalUtilPct != c.ExternalUtilPct || a.OwnUtilPct != c.OwnUtilPct {
		t.Fatalf("round trip changed split: %+v vs %+v", a, c)
	}
}

func TestSplitWithoutPerProcessDataTreatsAllAsExternal(t *testing.T) {
	s := Sample{
		DedicatedTotalBytes: 16 * gib,
		DedicatedUsedBytes:  13 * gib,
		Engines:             []Engine{{Key: "phys_0_eng_0", Type: "3D", UtilPct: 60}},
	}
	d := Split(s, map[uint32]bool{100: true})
	if d.Attributed || d.ExternalUtilPct != 60 || d.ExternalDedicatedBytes != 13*gib {
		t.Fatalf("%+v", d)
	}
}

func TestFormatLUID(t *testing.T) {
	if got := FormatLUID(0, 0xC3F1); got != gpu {
		t.Fatalf("got %s", got)
	}
}
