//go:build !windows

package service

import "errors"

func (s *Service) requestRestart() error {
	return errors.New("restart from the API is available only for the Windows service")
}
