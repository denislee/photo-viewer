// Package format holds small display formatters shared by the GUI and the web UI.
package format

import "fmt"

// Bytes formats a byte count for display. Units are binary (1024-based), so the
// suffix is the accurate "KiB"/"MiB"/… rather than the decimal "KB"/"MB"; below
// 1 KiB the raw byte count is printed.
func Bytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
