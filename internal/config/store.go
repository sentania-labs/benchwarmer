package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Parse decodes and validates a config document. Unknown fields are
// rejected so a typo cannot silently fall back to a default.
func Parse(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("parse config: trailing data after JSON document")
	}
	upgrade(&c)
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// upgrade fills fields added to schema version 1 after a config file may
// have been written, with the value that preserves earlier behavior or the
// shipped default. Without this, an upgrade would make an existing file fail
// validation and the service would run on factory defaults.
func upgrade(c *Config) {
	if c.Runtime.RunAs == "" {
		c.Runtime.RunAs = RunAsLocalService
	}
	if c.Runtime.StopMode == "" {
		c.Runtime.StopMode = StopGraceful
	}
	if c.Runtime.GracefulStopTimeout == 0 {
		c.Runtime.GracefulStopTimeout = Duration(5 * time.Second)
	}
	// PFX files are no longer read. A leftover password file is dropped
	// unless it goes with a PFX certificate, which the certificate loader
	// then reports (only the inference listener stops).
	if !isPFXPath(c.Listen.InferenceTLS.CertFile) {
		c.Listen.InferenceTLS.LegacyPFXPasswordFile = ""
	}
	if c.Listen.InferenceTLS.SelfSignedHosts == nil { // added with inference_tls
		c.Listen.InferenceTLS.SelfSignedHosts = []string{}
	}
}

// Marshal renders a config document.
func Marshal(c Config) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Clone deep-copies a config.
func Clone(c Config) Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // Config contains only JSON-safe types
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// WriteFileAtomic writes data so readers see either the old or the new file,
// never a partial one: temp file in the same directory, fsync, rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	// os.Rename replaces an existing file on Windows (MoveFileEx with
	// MOVEFILE_REPLACE_EXISTING) as well as on Unix.
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

// LoadResult says where the active config came from.
type LoadResult struct {
	Config Config
	// Source is "primary", "last_good", or "default".
	Source string
	// PrimaryErr explains why the primary file was not used, if it wasn't.
	PrimaryErr error
}

// Store persists the active config and a last-known-good copy.
type Store struct {
	mu       sync.Mutex
	path     string
	lastGood string
}

// NewStore manages config at path; the last-good copy sits beside it.
func NewStore(path string) *Store {
	ext := filepath.Ext(path)
	return &Store{path: path, lastGood: path[:len(path)-len(ext)] + ".last-good" + ext}
}

// Path returns the primary config path.
func (s *Store) Path() string { return s.path }

// Load reads the primary config, falling back to the last-good copy and then
// to defaults. A missing primary on first run writes the defaults.
func (s *Store) Load() (LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err == nil {
		c, perr := Parse(b)
		if perr == nil {
			// Keep last-good in step with a primary that validates.
			if lg, err := os.ReadFile(s.lastGood); err != nil || !bytes.Equal(lg, b) {
				_ = WriteFileAtomic(s.lastGood, b, 0o600)
			}
			return LoadResult{Config: c, Source: "primary"}, nil
		}
		err = perr
	} else if errors.Is(err, os.ErrNotExist) {
		c := Default()
		out, merr := Marshal(c)
		if merr != nil {
			return LoadResult{}, merr
		}
		if werr := WriteFileAtomic(s.path, out, 0o600); werr != nil {
			return LoadResult{}, werr
		}
		_ = WriteFileAtomic(s.lastGood, out, 0o600)
		return LoadResult{Config: c, Source: "default"}, nil
	}
	primaryErr := err
	if lb, lerr := os.ReadFile(s.lastGood); lerr == nil {
		if c, perr := Parse(lb); perr == nil {
			return LoadResult{Config: c, Source: "last_good", PrimaryErr: primaryErr}, nil
		}
	}
	// Neither file is usable. Run on defaults but do not overwrite the
	// operator's broken file; they may want to repair it.
	return LoadResult{Config: Default(), Source: "default", PrimaryErr: primaryErr}, nil
}

// Save validates c and writes it atomically as both primary and last-good.
func (s *Store) Save(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	b, err := Marshal(c)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := WriteFileAtomic(s.path, b, 0o600); err != nil {
		return err
	}
	return WriteFileAtomic(s.lastGood, b, 0o600)
}
