//go:build !windows

package tlscert

import (
	"crypto/tls"
	"errors"
)

func storeCertificate(string, string) (*tls.Certificate, error) {
	return nil, errors.New("tls: the Windows certificate store is only available on Windows")
}

// ListStore reports that there is no certificate store off Windows.
func ListStore() ([]StoreCert, error) {
	return nil, errors.New("tls: the Windows certificate store is only available on Windows")
}
