//go:build !windows

package main

import (
	"errors"

	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

type gpuSource interface {
	Collect() telemetry.Sample
	Close()
}

var errNoGPU = errors.New("GPU telemetry is only implemented on Windows")

func newGPUSource(string) (gpuSource, error) { return nil, errNoGPU }

func listAdapters() (any, error) { return nil, errNoGPU }
