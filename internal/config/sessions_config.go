package config

import (
	"errors"
	"os"
)

// ReadSessionsConfig reads the sessions settings from the config file at path,
// which callers pass as the user config. A missing file is an empty config. See
// SessionsConfig for why nothing else supplies these settings.
func ReadSessionsConfig(path string) (SessionsConfig, error) {
	cfg, err := loadConfigFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionsConfig{}, nil
		}
		return SessionsConfig{}, err
	}
	return cfg.Sessions, nil
}
