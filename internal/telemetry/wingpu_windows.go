//go:build windows

package telemetry

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modpdh                          = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW               = modpdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = modpdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData         = modpdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArray = modpdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery               = modpdh.NewProc("PdhCloseQuery")

	modgdi32                      = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTOpenAdapterFromLuid = modgdi32.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo    = modgdi32.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter        = modgdi32.NewProc("D3DKMTCloseAdapter")

	moddxgi                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = moddxgi.NewProc("CreateDXGIFactory1")
)

const (
	pdhFmtDouble    = 0x00000200
	pdhFmtNoCap100  = 0x00008000
	pdhMoreData     = 0x800007D2
	pdhNoData       = 0x800007D5
	pdhCstatusValid = 0x0
	pdhCstatusNew   = 0x1
)

type pdhCounterSet struct {
	path string
	h    uintptr
}

// PDHCollector samples the Windows GPU performance counters.
type PDHCollector struct {
	query    uintptr
	counters []*pdhCounterSet
	built    time.Time
	primed   bool
}

var gpuCounterPaths = []string{
	`\GPU Engine(*)\Utilization Percentage`,
	`\GPU Process Memory(*)\Dedicated Usage`,
	`\GPU Process Memory(*)\Shared Usage`,
	`\GPU Adapter Memory(*)\Dedicated Usage`,
	`\GPU Adapter Memory(*)\Shared Usage`,
}

// NewPDHCollector opens a PDH query over the GPU counter sets.
func NewPDHCollector() (*PDHCollector, error) {
	c := &PDHCollector{}
	if err := c.build(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *PDHCollector) build() error {
	c.Close()
	var q uintptr
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return fmt.Errorf("PdhOpenQuery: 0x%x", r)
	}
	c.query = q
	c.counters = nil
	for _, p := range gpuCounterPaths {
		cs := &pdhCounterSet{path: p}
		pp, _ := windows.UTF16PtrFromString(p)
		if r, _, _ := procPdhAddEnglishCounterW.Call(q, uintptr(unsafe.Pointer(pp)), 0, uintptr(unsafe.Pointer(&cs.h))); r != 0 {
			c.Close()
			return fmt.Errorf("PdhAddEnglishCounter %s: 0x%x", p, r)
		}
		c.counters = append(c.counters, cs)
	}
	c.built = time.Now()
	c.primed = false
	return nil
}

// Rebuild recreates the query. Useful after resume or driver reset, when
// counter instances may have been torn down.
func (c *PDHCollector) Rebuild() error { return c.build() }

// Collect samples all counter sets. The first call after (re)building only
// primes rate counters; utilization values appear from the second call.
func (c *PDHCollector) Collect() (RawCounters, error) {
	if c.query == 0 {
		return RawCounters{}, errors.New("pdh query closed")
	}
	if r, _, _ := procPdhCollectQueryData.Call(c.query); r != 0 && r != pdhNoData {
		return RawCounters{}, fmt.Errorf("PdhCollectQueryData: 0x%x", r)
	}
	wasPrimed := c.primed
	c.primed = true
	out := RawCounters{}
	var errs []string
	for i, cs := range c.counters {
		m, err := formattedArray(cs.h)
		if err != nil {
			// A counter set with zero instances reports an error; that is data,
			// not a failure, except for the adapter set which must exist.
			if i >= 3 {
				errs = append(errs, cs.path+": "+err.Error())
			}
			m = map[string]float64{}
		}
		switch i {
		case 0:
			if !wasPrimed {
				m = map[string]float64{}
			}
			out.EngineUtil = m
		case 1:
			out.ProcDedicated = m
		case 2:
			out.ProcShared = m
		case 3:
			out.AdapterDedicated = m
		case 4:
			out.AdapterShared = m
		}
	}
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// Primed reports whether utilization values are available yet.
func (c *PDHCollector) Primed() bool { return c.primed }

// Close releases the PDH query.
func (c *PDHCollector) Close() {
	if c.query != 0 {
		procPdhCloseQuery.Call(c.query)
		c.query = 0
	}
}

// pdhFmtCounterValueItemDouble mirrors PDH_FMT_COUNTERVALUE_ITEM_W with a
// double value on 64-bit Windows.
type pdhFmtCounterValueItemDouble struct {
	Name    *uint16
	CStatus uint32
	_       uint32
	Value   float64
}

func formattedArray(h uintptr) (map[string]float64, error) {
	var size, count uint32
	r, _, _ := procPdhGetFormattedCounterArray.Call(h, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if r != pdhMoreData {
		if r == 0 {
			return map[string]float64{}, nil
		}
		return nil, fmt.Errorf("PdhGetFormattedCounterArray size: 0x%x", r)
	}
	// Allocate as uint64s for 8-byte alignment; names point into the buffer.
	buf := make([]uint64, (size+7)/8+1)
	r, _, _ = procPdhGetFormattedCounterArray.Call(h, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray: 0x%x", r)
	}
	items := unsafe.Slice((*pdhFmtCounterValueItemDouble)(unsafe.Pointer(&buf[0])), count)
	out := make(map[string]float64, count)
	for _, it := range items {
		if it.CStatus != pdhCstatusValid && it.CStatus != pdhCstatusNew {
			continue
		}
		// Several instances can share a name; sum them.
		out[windows.UTF16PtrToString(it.Name)] += it.Value
	}
	return out, nil
}

// Adapter describes a DXGI adapter.
type Adapter struct {
	Name           string `json:"name"`
	VendorID       uint32 `json:"vendor_id"`
	DeviceID       uint32 `json:"device_id"`
	LUID           string `json:"luid"`
	luidLow        uint32
	luidHigh       int32
	DedicatedBytes uint64 `json:"dedicated_bytes"`
	SharedBytes    uint64 `json:"shared_bytes"`
	Software       bool   `json:"software"`
	// LocalBudgetBytes and LocalUsageBytes come from IDXGIAdapter3 and are
	// scoped to the calling process (budget reflects system-wide pressure).
	LocalBudgetBytes uint64 `json:"local_budget_bytes"`
	LocalUsageBytes  uint64 `json:"local_usage_bytes"`
}

var (
	iidIDXGIFactory1 = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIAdapter3 = windows.GUID{Data1: 0x645967a4, Data2: 0x1392, Data3: 0x4310, Data4: [8]byte{0xa7, 0x98, 0x80, 0x53, 0xce, 0x3e, 0x93, 0xfd}}
)

type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	LuidLow               uint32
	LuidHigh              int32
	Flags                 uint32
}

type dxgiQueryVideoMemoryInfo struct {
	Budget                  uint64
	CurrentUsage            uint64
	AvailableForReservation uint64
	CurrentReservation      uint64
}

// comCall invokes vtable slot index on a COM object.
func comCall(obj unsafe.Pointer, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func comRelease(obj unsafe.Pointer) { comCall(obj, 2) }

// Adapters enumerates DXGI adapters.
func Adapters() ([]Adapter, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	var factory unsafe.Pointer
	if r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); r != 0 {
		return nil, fmt.Errorf("CreateDXGIFactory1: 0x%x", r)
	}
	defer comRelease(factory)
	var out []Adapter
	for i := uintptr(0); ; i++ {
		var ad unsafe.Pointer
		// IDXGIFactory1::EnumAdapters1 is vtable slot 12.
		if r := comCall(factory, 12, i, uintptr(unsafe.Pointer(&ad))); r != 0 {
			break // DXGI_ERROR_NOT_FOUND ends the list
		}
		var d dxgiAdapterDesc1
		// IDXGIAdapter1::GetDesc1 is slot 10.
		if r := comCall(ad, 10, uintptr(unsafe.Pointer(&d))); r == 0 {
			a := Adapter{
				Name:           windows.UTF16ToString(d.Description[:]),
				VendorID:       d.VendorID,
				DeviceID:       d.DeviceID,
				LUID:           FormatLUID(d.LuidHigh, d.LuidLow),
				luidLow:        d.LuidLow,
				luidHigh:       d.LuidHigh,
				DedicatedBytes: uint64(d.DedicatedVideoMemory),
				SharedBytes:    uint64(d.SharedSystemMemory),
				Software:       d.Flags&2 != 0, // DXGI_ADAPTER_FLAG_SOFTWARE
			}
			var ad3 unsafe.Pointer
			if comCall(ad, 0, uintptr(unsafe.Pointer(&iidIDXGIAdapter3)), uintptr(unsafe.Pointer(&ad3))) == 0 {
				var mi dxgiQueryVideoMemoryInfo
				// IDXGIAdapter3::QueryVideoMemoryInfo is slot 14; group 0 is local.
				if comCall(ad3, 14, 0, 0, uintptr(unsafe.Pointer(&mi))) == 0 {
					a.LocalBudgetBytes, a.LocalUsageBytes = mi.Budget, mi.CurrentUsage
				}
				comRelease(ad3)
			}
			out = append(out, a)
		}
		comRelease(ad)
	}
	return out, nil
}

// PerfData is D3DKMT adapter performance data (what Task Manager uses for
// GPU temperature). Fields the driver does not report are zero.
type PerfData struct {
	TemperatureC    float64 `json:"temperature_c"`
	PowerPct        float64 `json:"power_pct"`
	FanRPM          uint32  `json:"fan_rpm"`
	MemoryFreqHz    uint64  `json:"memory_freq_hz"`
	PCIEBandwidth   uint64  `json:"pcie_bandwidth"`
	MemoryBandwidth uint64  `json:"memory_bandwidth"`
}

type d3dkmtOpenAdapterFromLuid struct {
	LuidLow  uint32
	LuidHigh int32
	HAdapter uint32
}

type d3dkmtQueryAdapterInfo struct {
	HAdapter uint32
	Type     uint32
	Data     uintptr
	Size     uint32
	_        uint32
}

type d3dkmtAdapterPerfData struct {
	PhysicalAdapterIndex uint32
	_                    uint32
	MemoryFrequency      uint64
	MaxMemoryFrequency   uint64
	MaxMemoryFrequencyOC uint64
	MemoryBandwidth      uint64
	PCIEBandwidth        uint64
	FanRPM               uint32
	Power                uint32 // tenths of a percent
	Temperature          uint32 // deci-Celsius
	PowerStateOverride   uint8
	_                    [3]byte
}

const kmtqaitypeAdapterPerfData = 62

// QueryPerfData reads adapter perf data for the adapter with the given LUID.
func QueryPerfData(a Adapter) (PerfData, error) {
	if err := procD3DKMTOpenAdapterFromLuid.Find(); err != nil {
		return PerfData{}, err
	}
	open := d3dkmtOpenAdapterFromLuid{LuidLow: a.luidLow, LuidHigh: a.luidHigh}
	if r, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); r != 0 {
		return PerfData{}, fmt.Errorf("D3DKMTOpenAdapterFromLuid: NTSTATUS 0x%x", r)
	}
	defer procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&open.HAdapter)))
	var pd d3dkmtAdapterPerfData
	q := d3dkmtQueryAdapterInfo{HAdapter: open.HAdapter, Type: kmtqaitypeAdapterPerfData,
		Data: uintptr(unsafe.Pointer(&pd)), Size: uint32(unsafe.Sizeof(pd))}
	if r, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); r != 0 {
		return PerfData{}, fmt.Errorf("D3DKMTQueryAdapterInfo(ADAPTERPERFDATA): NTSTATUS 0x%x", r)
	}
	return PerfData{
		TemperatureC:    float64(pd.Temperature) / 10,
		PowerPct:        float64(pd.Power) / 10,
		FanRPM:          pd.FanRPM,
		MemoryFreqHz:    pd.MemoryFrequency,
		PCIEBandwidth:   pd.PCIEBandwidth,
		MemoryBandwidth: pd.MemoryBandwidth,
	}, nil
}

// WindowsCollector combines PDH, DXGI and D3DKMT into Samples for one adapter.
type WindowsCollector struct {
	pdh     *PDHCollector
	adapter Adapter
}

// NewWindowsCollector selects the adapter by LUID, or by name substring when
// selector does not look like a LUID, or the largest discrete adapter when
// selector is empty.
func NewWindowsCollector(selector string) (*WindowsCollector, error) {
	ads, err := Adapters()
	if err != nil {
		return nil, err
	}
	a, err := SelectAdapter(ads, selector)
	if err != nil {
		return nil, err
	}
	p, err := NewPDHCollector()
	if err != nil {
		return nil, err
	}
	return &WindowsCollector{pdh: p, adapter: a}, nil
}

// SelectAdapter picks the target adapter from an enumeration.
func SelectAdapter(ads []Adapter, selector string) (Adapter, error) {
	sel := strings.ToLower(selector)
	var best *Adapter
	for i := range ads {
		a := &ads[i]
		if a.Software {
			continue
		}
		if sel != "" {
			if strings.ToLower(a.LUID) == sel || strings.Contains(strings.ToLower(a.Name), sel) {
				return *a, nil
			}
			continue
		}
		if best == nil || a.DedicatedBytes > best.DedicatedBytes {
			best = a
		}
	}
	if best == nil {
		return Adapter{}, fmt.Errorf("no hardware adapter matches %q", selector)
	}
	return *best, nil
}

// Adapter returns the selected adapter.
func (w *WindowsCollector) Adapter() Adapter { return w.adapter }

// Rebuild recreates the counter query, e.g. after resume.
func (w *WindowsCollector) Rebuild() error { return w.pdh.Rebuild() }

// Collect takes one Sample.
func (w *WindowsCollector) Collect() Sample {
	t0 := time.Now()
	raw, err := w.pdh.Collect()
	s := FromCounters(raw, w.adapter.LUID)
	s.Time = t0
	s.AdapterName = w.adapter.Name
	s.DedicatedTotalBytes = w.adapter.DedicatedBytes
	s.Complete = w.pdh.Primed()
	if err != nil {
		s.Complete = false
		s.Errors = append(s.Errors, err.Error())
	}
	if pd, err := QueryPerfData(w.adapter); err == nil {
		if pd.TemperatureC > 0 {
			t := pd.TemperatureC
			s.TemperatureC = &t
		}
		p, f := pd.PowerPct, pd.FanRPM
		s.PowerPct, s.FanRPM = &p, &f
	} else {
		s.Errors = append(s.Errors, err.Error())
	}
	s.CollectDuration = time.Since(t0)
	return s
}

// Close releases resources.
func (w *WindowsCollector) Close() { w.pdh.Close() }
