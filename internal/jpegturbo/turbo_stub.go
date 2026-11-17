//go:build !cgo || nojpegturbo

package jpegturbo

import "image"

const available = false

func decodeFit([]byte, func(w, h int) (minW, minH int)) (*image.RGBA, Config, error) {
	return nil, Config{}, ErrUnavailable
}
