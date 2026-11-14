//go:build !windows

package opconf

import (
	"fmt"
	"os"
	"path/filepath"
)

// platformConfigDir: linux $XDG_CONFIG_HOME or ~/.config; darwin ~/Library/Application Support.
func platformConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "mirafleet"), nil
}

// platformStateDir: linux os.UserCacheDir; darwin ~/Library/Caches.
func platformStateDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return filepath.Join(base, "mirafleet"), nil
}
