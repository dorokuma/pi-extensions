module github.com/dorokuma/pi-extensions/go/pi-my-compact-ui

go 1.26

// PiG resolves this requirement to the version-matched staged SDK at build time.
// Local build/vet/test resolve it through go/go.work -> go/pigsdk.
require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
