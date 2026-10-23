package model

import "time"

// ArtifactID is the firmware cache primary key.
type ArtifactID int64

// Artifact is one cached firmware file. Artifacts are immutable once
// stored; a new upstream build is a new artifact.
type Artifact struct {
	ID        ArtifactID
	Release   string     // upstream release version, e.g. "16.0.0"
	Filename  string     // upstream file name, e.g. "tasmota-sensors.bin.gz", "tasmota32c3.bin"
	Family    ChipFamily // family proven by the image header (and, for solo1, the filename)
	Variant   string     // normalised variant, see Identity.Variant
	Language  string
	Safeboot  bool // a tasmota32*-safeboot.bin image; never served as an OTA target
	Size      int64
	SHA256    string // hex
	Source    string // configured source name, e.g. "github", "ota.tasmota.com"
	SourceURL string
	FetchedAt time.Time
	// Custom is empty for upstream files. For an uploaded image (and the minimal
	// copy kept with it) it is the key of the upload (first hex digits of its
	// SHA-256): the same release and file name may exist upstream and as any
	// number of uploads, told apart by this key. Devices fetch such a file at
	// /c/<Custom>/fw/<Release>/<Filename>.
	Custom string
}
