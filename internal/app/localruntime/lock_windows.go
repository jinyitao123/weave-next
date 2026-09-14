//go:build windows

package localruntime

import "errors"

func lockHostRoot(string) (func(), error) {
	return nil, errors.New("managed local runtime is unavailable on this platform")
}
