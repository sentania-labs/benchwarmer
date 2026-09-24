//go:build !windows

package service

import (
	"errors"

	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

func newPlatformTelemetry(string) (telemetry.Source, error) {
	return nil, errors.New("GPU telemetry is only implemented on Windows; use --simulate-gpu for development")
}
