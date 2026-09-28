//go:build !linux

package ui

import "errors"

// blockEvents has no event source off Linux; the watcher polls instead.
func blockEvents(stop <-chan struct{}) (<-chan struct{}, error) {
	return nil, errors.New("block device events unsupported on this platform")
}
