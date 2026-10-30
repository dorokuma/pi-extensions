module github.com/dorokuma/pi-extensions/go/jev-gate/cmd/jev-mcp

go 1.26

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
	github.com/dorokuma/pi-extensions/go/jev-gate v0.0.0
)

replace (
	github.com/MichaelKinsy/PiG/extensions/sdk => ../../../pigsdk
	github.com/dorokuma/pi-extensions/go/jev-gate => ../..
)
