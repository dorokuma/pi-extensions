// Package pigsdkregress holds regression tests for the go/pigsdk SDK mirror.
// go/pigsdk must stay byte-identical to the staged PiG SDK (see make
// sdk-check), so tests that exercise it live here in a nested module and run
// under make test like every other component. The package intentionally
// contains only *_test.go files exercising the exported SDK surface.
package pigsdkregress
