//go:build windows

package opconf

import "os"

func platformConfigDir() (string, error) { return windowsConfigDir(os.Getenv) }

func platformStateDir() (string, error) { return windowsStateDir(os.Getenv) }
