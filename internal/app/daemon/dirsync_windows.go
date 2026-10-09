//go:build windows

package daemon

// syncDir is a no-op on Windows: directory handles cannot be flushed there
// ("Access is denied"), and NTFS journals the rename metadata itself.
func syncDir(string) error { return nil }
