// Package deploy provides the shipped service-unit templates so the binary can
// render them (nenya service-unit) from the same files that are packaged
// (CONTRACT.md §4.5, §8).
package deploy

import _ "embed"

// SystemdService is the shipped systemd service unit (deploy/nenya.service).
//
//go:embed nenya.service
var SystemdService string

// LaunchdPlist is the shipped launchd property list (deploy/nenya.plist).
//
//go:embed nenya.plist
var LaunchdPlist string
