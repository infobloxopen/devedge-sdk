// config/koanf is a NESTED Go module (WS-011 / F039): the koanf config-file
// adapter lives here, not in the root module's dependency graph. The module
// path equals the import path, so no .go import statement changes — a consumer
// pulls koanf only when it `require`s THIS module.
//
// It requires the root devedge-sdk module (the adapter implements the core
// config.Source seam). The local go.work at the repo root resolves that require
// to the working tree during dev/CI; the require below is the version a
// published consumer resolves, bumped by the synchronized release script (P3).
module github.com/infobloxopen/devedge-sdk/config/koanf

go 1.25.5

require (
	github.com/infobloxopen/devedge-sdk v0.30.0
	github.com/knadh/koanf/parsers/yaml v1.1.0
	github.com/knadh/koanf/providers/file v1.2.1
	github.com/knadh/koanf/v2 v2.3.5
)

require (
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sys v0.45.0 // indirect
)
