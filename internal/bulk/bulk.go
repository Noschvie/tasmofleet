// Package bulk sends commands to many devices (M2). Commands are checked
// against an allowlist of command names (configurable; default read-only
// commands plus Power, Restart 1, and the policy set). Anything outside the
// list, and any batch larger than one device, needs an explicit
// confirmation token issued by a preview step. Results are collected per
// device and audited.
package bulk

// DefaultAllowlist are command names allowed without editing configuration.
var DefaultAllowlist = []string{
	"Status", "State", "Power", "Restart", "Timezone", "TimeDst", "TimeStd",
	"NtpServer", "PowerOnState", "SetOption0", "SaveData", "TelePeriod",
}
