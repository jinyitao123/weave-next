package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// CheckOrganizationConnectionFile explains why the configured organization
// connection file could not be served, for a deployment check before startup.
// It names the file and the cause, never its contents.
func CheckOrganizationConnectionFile(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist; create the file on the host and mount it read-only, the mount must not create it", path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s cannot be reached by this process (uid %d)", path, os.Getuid())
	case err != nil:
		return fmt.Errorf("%s cannot be inspected: %v", path, err)
	case info.IsDir():
		return fmt.Errorf("%s is a directory, not a file; its host file was probably missing when the container was created", path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case info.Size() > 8192:
		return fmt.Errorf("%s is larger than 8 KiB", path)
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%s is not readable by this process (uid %d); grant that user read access", path, os.Getuid())
	}
	if err != nil {
		return fmt.Errorf("%s cannot be opened: %v", path, err)
	}
	_ = file.Close()
	if _, err := readOrganizationConnection(path); err != nil {
		return fmt.Errorf("%s is not a valid organization connection: it must hold exactly version 1, forgeOrigin and weaveOrigin, each origin http or https with no path, query or credentials (%v)", path, err)
	}
	return nil
}
