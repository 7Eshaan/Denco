//go:build !windows

package cli

import "errors"

// registryPath and deleteAfterExit exist only on Windows (setup_windows.go).
type registryPath struct{ pathEditor }

func deleteAfterExit(string, string) error { return errors.New("only on Windows") }
