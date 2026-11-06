package config

import (
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/schedule"
)

// BackupSchedule is the scheduled device backup as a schedule (#601). A value that does not parse
// cannot be stored (validate refuses it), so the zero schedule it falls back to means manual only.
func (c Config) BackupSchedule() schedule.Schedule {
	s, err := schedule.New(strings.Join(c.BackupDays, ","), c.BackupTime)
	if err != nil {
		return schedule.Schedule{}
	}
	return s
}

// SnapshotSchedule is the scheduled database snapshot as a schedule (#602); like BackupSchedule, a
// value that does not parse means off.
func (c Config) SnapshotSchedule() schedule.Schedule {
	s, err := schedule.New(strings.Join(c.SnapshotDays, ","), c.SnapshotTime)
	if err != nil {
		return schedule.Schedule{}
	}
	return s
}

// canonicalDays is the weekday list as it is stored and shown: lower case, Monday first, each day
// once. The parser takes "Wed, MON"; stored like that, the Backups page would show its chips
// unticked and the next save of the page would turn the schedule off. A list that does not parse
// stays as it is, for validate to refuse.
func canonicalDays(days []string) []string {
	d, err := schedule.ParseDays(strings.Join(days, ","))
	if err != nil {
		return days
	}
	return splitList(d.String())
}
