package spooler

import (
	"errors"
	"fmt"
	"os"
)

type Config struct {
	Dir       string
	Pattern   string
	ForceFail bool
}

type Spooler struct {
	f *os.File
}

func (s *Spooler) Write(p []byte) (int, error) {
	return s.f.Write(p)
}

func (s *Spooler) Path() string {
	return s.f.Name()
}

func Open(cfg Config) (*Spooler, func() error, error) {
	pattern := cfg.Pattern
	if pattern == "" {
		pattern = "spool-*.log"
	}
	f, err := os.CreateTemp(cfg.Dir, pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("create spool file: %w", err)
	}
	if cfg.ForceFail {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, nil, fmt.Errorf("initialize spooler: %w", errors.New("forced failure"))
	}
	s := &Spooler{f: f}
	cleanup := func() error {
		return errors.Join(f.Close(), os.Remove(f.Name()))
	}
	return s, cleanup, nil
}
