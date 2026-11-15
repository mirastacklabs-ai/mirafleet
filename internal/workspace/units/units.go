// Package units parses and formats the human units of a workspace fleet's configuration
// ("4GiB", "512MiB", "0.5" CPUs, "24h"). It is pure (no flags, no I/O) so the CLI and the admin
// GUI accept exactly the same text. Errors are plain sentences without a flag name.
package units

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sizeMBRE = regexp.MustCompile(`^([0-9]{1,9})\s*(MiB|GiB|TiB)?$`)

// ParseSizeMB parses "4GiB", "512MiB" or a bare number of MiB into whole MiB.
func ParseSizeMB(s string) (int, error) {
	m := sizeMBRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("want a size such as 512MiB or 4GiB")
	}
	n, _ := strconv.Atoi(m[1])
	mult := map[string]int{"": 1, "MiB": 1, "GiB": 1 << 10, "TiB": 1 << 20}[m[2]]
	if n < 1 || n > math.MaxInt32/mult {
		return 0, fmt.Errorf("size is out of range")
	}
	return n * mult, nil
}

// FormatSizeMB renders whole MiB with the largest exact unit (4096 -> "4GiB").
func FormatSizeMB(mb int) string {
	switch {
	case mb >= 1<<20 && mb%(1<<20) == 0:
		return fmt.Sprintf("%dTiB", mb>>20)
	case mb >= 1<<10 && mb%(1<<10) == 0:
		return fmt.Sprintf("%dGiB", mb>>10)
	}
	return fmt.Sprintf("%dMiB", mb)
}

// ParseCPUMilli parses a CPU count such as "2" or "0.5" into millicpu. max is the largest
// number of CPUs accepted.
func ParseCPUMilli(s string, max int) (int, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	// The negated form also rejects NaN. Below 0.001 the value would round to 0 ("default").
	if err != nil || !(f >= 0.001 && f <= float64(max)) {
		return 0, fmt.Errorf("want a number of CPUs between 0.001 and %d", max)
	}
	return int(math.Round(f * 1000)), nil
}

// FormatCPUMilli renders millicpu as CPUs (2000 -> "2", 500 -> "0.5").
func FormatCPUMilli(m int) string {
	return strconv.FormatFloat(float64(m)/1000, 'f', -1, 64)
}

// ParseSeconds parses a Go duration such as "90s" or "24h" that is a whole number of seconds,
// at least one second and at most limit.
func ParseSeconds(s string, limit time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < time.Second || d%time.Second != 0 || d > limit {
		return 0, fmt.Errorf("want a whole number of seconds from 1s to %s, such as 90s or 24h", limit)
	}
	return d, nil
}

// FormatDuration renders a whole-second duration compactly (24h, 90m, 45s).
func FormatDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
