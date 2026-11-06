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
