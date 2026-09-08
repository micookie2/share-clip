//go:build !windows && !linux

package clipboard

import "errors"

// NewBackend reports that clipboard sharing is unsupported on this platform.
func NewBackend(pollIntervalMs int) (Backend, error) {
	return nil, errors.New("share-clip clipboard backend supports Windows and Linux only")
}
