package tasmota

// StatusFWR is the StatusFWR object of "Status 2".
type StatusFWR struct {
	Version       string `json:"Version"` // "16.0.0(release-tasmota)", "16.0.0(release-tasmota32)"
	BuildDateTime string `json:"BuildDateTime"`
	Core          string `json:"Core"`
	SDK           string `json:"SDK"`
	CPUFrequency  int    `json:"CpuFrequency"`
	Hardware      string `json:"Hardware"` // "ESP8266EX", "ESP8285N08", "ESP32-D0WD-V3", "ESP32-C3", ...
	CR            string `json:"CR"`
}

// StatusMEM is the StatusMEM object of "Status 4".
type StatusMEM struct {
	ProgramSize      int    `json:"ProgramSize"`
	Free             int    `json:"Free"`
	Heap             int    `json:"Heap"`
	ProgramFlashSize int    `json:"ProgramFlashSize"`
	FlashSize        int    `json:"FlashSize"`
	FlashChipID      string `json:"FlashChipId"`
	FlashFrequency   int    `json:"FlashFrequency"`
	FlashMode        string `json:"FlashMode"`
}

// StatusSTS is the StatusSTS object of "Status 11" (same shape as tele STATE).
type StatusSTS struct {
	Time      string            `json:"Time"`
	Uptime    string            `json:"Uptime"`
	UptimeSec int64             `json:"UptimeSec"`
	Heap      int               `json:"Heap"`
	Wifi      map[string]any    `json:"Wifi"`
	Power     map[string]string `json:"-"` // POWER, POWER1..n collected by ParseStatus11
}

// UpgradeProbe is the answer to "Upgrade" without parameter.
type UpgradeProbe struct {
	// Idle is true when the answer has the form "1 or >X to upgrade" (no
	// upgrade started); Version is then X. Normal AND minimal firmware answer
	// this way (OBS: all fixtures), so it says nothing about minimal. Minimal
	// is detected by fwselect.Detect: Status 2 Unknown plus RootInfo.
	Idle    bool
	Version string
}
