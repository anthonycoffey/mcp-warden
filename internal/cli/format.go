package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// humanBytes formats a byte count compactly: 512 B, 12 MB, 1.4 GB.
func humanBytes(n uint64) string {
	switch {
	case n == 0:
		return "-"
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}

// humanAge formats a duration the way people say it: 45s, 12m, 3h, 2d.
func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// parseSince accepts Go durations plus a day suffix: 90m, 24h, 7d.
func parseSince(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
