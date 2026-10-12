package history

import (
	"cmp"
	"slices"
	"time"
)

// DayCount is the number of trees that became orphans on one calendar day.
type DayCount struct {
	Day     string `json:"day"` // YYYY-MM-DD, local time
	Orphans int    `json:"orphans"`
}

// CommandStats aggregates orphan events by command line.
type CommandStats struct {
	Command string `json:"command"`
	Orphans int    `json:"orphans"`
	Killed  int    `json:"killed"`
	PeakRSS uint64 `json:"peakRss"`
}

// Summary answers "what has been leaking?" over a time range.
type Summary struct {
	Since           time.Time      `json:"since"`
	Events          int            `json:"events"`
	Orphans         int            `json:"orphans"`
	Killed          int            `json:"killed"`
	Days            []DayCount     `json:"days"`
	TopCommands     []CommandStats `json:"topCommands"`
	PeakOrphanedRSS uint64         `json:"peakOrphanedRss"`
	PeakAt          time.Time      `json:"peakAt,omitzero"`
}

// Summarize aggregates events at or after since. Peak orphaned memory is the
// largest total RSS of simultaneously orphaned trees, replayed from the log.
func Summarize(events []Event, since time.Time, topN int) Summary {
	s := Summary{Since: since}
	days := map[string]int{}
	cmds := map[string]*CommandStats{}
	live := map[string]uint64{} // currently orphaned tree → RSS
	var liveTotal uint64

	for _, e := range events {
		if e.Time.Before(since) {
			continue
		}
		s.Events++
		switch {
		case e.IsOrphaning():
			s.Orphans++
			days[e.Time.Local().Format(time.DateOnly)]++
			c := stats(cmds, e.Command)
			c.Orphans++
			c.PeakRSS = max(c.PeakRSS, e.RSS)
			if _, ok := live[e.Key]; !ok {
				live[e.Key] = e.RSS
				liveTotal += e.RSS
			}
			if liveTotal > s.PeakOrphanedRSS {
				s.PeakOrphanedRSS, s.PeakAt = liveTotal, e.Time
			}
		case e.Type == Killed:
			s.Killed++
			stats(cmds, e.Command).Killed++
			liveTotal -= live[e.Key]
			delete(live, e.Key)
		case e.Type == Disappeared:
			liveTotal -= live[e.Key]
			delete(live, e.Key)
		}
	}

	for day, n := range days {
		s.Days = append(s.Days, DayCount{Day: day, Orphans: n})
	}
	slices.SortFunc(s.Days, func(a, b DayCount) int { return cmp.Compare(a.Day, b.Day) })

	for _, c := range cmds {
		if c.Orphans > 0 {
			s.TopCommands = append(s.TopCommands, *c)
		}
	}
	slices.SortFunc(s.TopCommands, func(a, b CommandStats) int {
		return cmp.Or(cmp.Compare(b.Orphans, a.Orphans), cmp.Compare(b.PeakRSS, a.PeakRSS), cmp.Compare(a.Command, b.Command))
	})
	if topN > 0 && len(s.TopCommands) > topN {
		s.TopCommands = s.TopCommands[:topN]
	}
	return s
}

func stats(m map[string]*CommandStats, cmd string) *CommandStats {
	c, ok := m[cmd]
	if !ok {
		c = &CommandStats{Command: cmd}
		m[cmd] = c
	}
	return c
}
