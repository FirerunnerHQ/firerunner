package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// MinEC2PoolKeyBytes is the shortest EC2 pool key accepted.
const MinEC2PoolKeyBytes = 32

// ReadEC2PoolKey returns the key in --ec2-pool-key-file, surrounding white
// space trimmed; nil and no error when none is configured. Startup and
// `config check` both call it, so they agree on every key. No error holds
// key material.
func (c *Config) ReadEC2PoolKey() ([]byte, error) {
	const flagName = "--ec2-pool-key-file"
	if c.EC2PoolKeyFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(c.EC2PoolKeyFile)
	if err != nil {
		why := "could not be read"
		switch {
		case errors.Is(err, os.ErrNotExist):
			why = "does not exist"
		case errors.Is(err, os.ErrPermission):
			why = "is not readable (permission denied)"
		}
		return nil, fmt.Errorf("%s %q %s", flagName, c.EC2PoolKeyFile, why)
	}
	key := bytes.TrimSpace(b)
	if len(key) < MinEC2PoolKeyBytes {
		return nil, fmt.Errorf("%s %q holds %d bytes, want at least %d", flagName, c.EC2PoolKeyFile, len(key), MinEC2PoolKeyBytes)
	}
	return key, nil
}
