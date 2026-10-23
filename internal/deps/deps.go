//go:build deps

// Package deps pins the third-party modules of the main module in go.mod
// and go.sum before the packages that use them are implemented, so parallel
// work packages never conflict on go.mod/go.sum. It is never compiled
// (build tag "deps"). Remove an import here once real code imports the module.
// The allowed module list is in docs/spec, section "Dependencies".
package deps

import (
	_ "github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/eclipse/paho.mqtt.golang"
	_ "github.com/mochi-mqtt/server/v2"
	_ "github.com/mochi-mqtt/server/v2/hooks/auth"
	_ "github.com/mochi-mqtt/server/v2/listeners"
	_ "github.com/mochi-mqtt/server/v2/packets"
	_ "golang.org/x/oauth2"
	_ "modernc.org/sqlite"
)
