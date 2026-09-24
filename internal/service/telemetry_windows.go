//go:build windows

package service

import "github.com/sentania-labs/benchwarmer/internal/telemetry"

func newPlatformTelemetry(adapter string) (telemetry.Source, error) {
	return telemetry.NewWindowsCollector(adapter)
}
