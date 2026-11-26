package runtime

import "fmt"

// HumanSize formats bytes like the docker CLI: decimal units, three
// significant digits ("7.8MB", "123kB").
func HumanSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.3g%s", f, units[i])
}
