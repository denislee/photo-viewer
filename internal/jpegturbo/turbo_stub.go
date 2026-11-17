//go:build !cgo || nojpegturbo

package jpegturbo

import "image"

const available = false

func decode([]byte, int, int) (*image.RGBA, error) { return nil, ErrUnavailable }

func decodeConfig([]byte) (Config, error) { return Config{}, ErrUnavailable }
