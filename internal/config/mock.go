package config

import _ "embed"

// MockYAML is the configuration of mock development mode.
//
//go:embed mock.yaml
var MockYAML []byte
