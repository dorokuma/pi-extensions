module github.com/dorokuma/pi-extensions/go/herdsman

go 1.26.0

// Pig resolves this requirement to the version-matched staged SDK at build time.
// No `replace`: a local path here would hijack Pig's staged SDK injection.
require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
