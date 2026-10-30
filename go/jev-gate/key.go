package jevgate

// Credential resolution of jev-gate (TS: KEY_ENV_NAMES / resolveApiKey /
// isLocalGatewayUrl / isLoopbackIpv4).
//
// Order (first non-blank wins):
//  1. the process environment MAGPIE_API_KEY / WINDHUB_API_KEY;
//  2. the `env` block of <agent dir>/settings.json — pi's settings.env is not
//     injected into the pi process environment, so the extension reads the file
//     itself and the key can stay in a 0600 file that is not version controlled;
//  3. the literal local-gateway token "magpie", but only when the base URL
//     points at a loopback gateway.
//
// A remote gateway without any credential resolves to nothing and the caller
// fails closed; there is no silent degradation.

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// isLoopbackIPv4 reports whether the host is an address of 127.0.0.0/8
// (TS: isLoopbackIpv4 — `net.isIPv4(ip) && ip.startsWith("127.")`).
func isLoopbackIPv4(host string) bool {
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Is4() {
		return false
	}
	octets := address.As4()
	return octets[0] == 127
}

// embeddedLoopbackIPv4 reads the IPv4 address embedded in an IPv6
// IPv4-mapped / IPv4-translated address and reports whether it is a loopback
// address. The three accepted layouts are the ones WHATWG URL normalisation can
// produce from `[::ffff:127.0.0.1]`, `[::ffff:0:127.0.0.1]`, and their expanded
// `[0:0:0:0:ffff:0:127.0.0.1]` spellings.
func embeddedLoopbackIPv4(address netip.Addr) bool {
	if !address.Is6() {
		return false
	}
	bytes := address.As16()
	// ::ffff:a.b.c.d — 10 zero bytes, then 0xffff, then the IPv4 address.
	mapped := true
	for i := 0; i < 10; i++ {
		if bytes[i] != 0 {
			mapped = false
			break
		}
	}
	if mapped && bytes[10] == 0xff && bytes[11] == 0xff {
		return bytes[12] == 127
	}
	// ::ffff:0:a.b.c.d — 8 zero bytes, 0xffff, one zero group, then IPv4.
	translated := true
	for i := 0; i < 8; i++ {
		if bytes[i] != 0 {
			translated = false
			break
		}
	}
	if translated && bytes[8] == 0xff && bytes[9] == 0xff && bytes[10] == 0 && bytes[11] == 0 {
		return bytes[12] == 127
	}
	return false
}

// IsLocalGatewayURL reports whether rawURL points at this machine's gateway
// (TS: isLocalGatewayUrl). Accepted: http/https, no userinfo, and a loopback
// host — localhost, localhost., ::1, 127.0.0.0/8, or the IPv4-mapped /
// IPv4-translated IPv6 spellings of those. Everything else, including a
// lookalike such as 127.0.0.1.evil.com, is remote.
func IsLocalGatewayURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	// A URL that carries credentials can never use the local-gateway fallback.
	if parsed.User != nil {
		if parsed.User.Username() != "" {
			return false
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return false
		}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return false
	}
	if strings.TrimSuffix(host, ".") == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if address.Is4() {
		return isLoopbackIPv4(host)
	}
	if address == netip.IPv6Loopback() {
		return true
	}
	return embeddedLoopbackIPv4(address)
}

// KeyLookup is the environment the credential resolution reads. Tests inject
// their own; the production value is DefaultKeyLookup.
type KeyLookup struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	HomeDir  func() (string, error)
}

// DefaultKeyLookup reads the real process environment, filesystem and home
// directory.
func DefaultKeyLookup() KeyLookup {
	return KeyLookup{
		Getenv:   os.Getenv,
		ReadFile: os.ReadFile,
		HomeDir:  os.UserHomeDir,
	}
}

// KeyResolution is the outcome of ResolveAPIKey.
type KeyResolution struct {
	// Key is the resolved credential; empty when OK is false.
	Key string
	// OK reports whether a credential was found.
	OK bool
	// Detail explains a failure in the shape the startup warning prints
	// (TS: keyLookupError).
	Detail string
}

// ResolveAPIKey resolves the gateway credential for baseURL
// (TS: resolveApiKey).
func ResolveAPIKey(baseURL string, lookup KeyLookup) KeyResolution {
	if lookup.Getenv == nil {
		lookup.Getenv = os.Getenv
	}
	if lookup.ReadFile == nil {
		lookup.ReadFile = os.ReadFile
	}
	if lookup.HomeDir == nil {
		lookup.HomeDir = os.UserHomeDir
	}

	for _, name := range keyEnvNames {
		if value := strings.TrimSpace(lookup.Getenv(name)); value != "" {
			return KeyResolution{Key: value, OK: true}
		}
	}

	directory := resolveAgentDirFor(lookup.Getenv, lookup.HomeDir)
	if directory == "" {
		directory = filepath.Join(".pig", "agent")
	}
	file := filepath.Join(directory, "settings.json")
	data, err := lookup.ReadFile(file)
	if err != nil {
		if IsLocalGatewayURL(baseURL) {
			return KeyResolution{Key: LocalGatewayKey, OK: true}
		}
		return KeyResolution{Detail: fmt.Sprintf("cannot read %s: %v", file, err)}
	}
	var settings map[string]any
	parseErr := json.Unmarshal(data, &settings)
	if parseErr != nil {
		if IsLocalGatewayURL(baseURL) {
			return KeyResolution{Key: LocalGatewayKey, OK: true}
		}
		return KeyResolution{Detail: fmt.Sprintf("cannot read %s: %v", file, parseErr)}
	}
	if env, ok := isRecord(settings["env"]); ok {
		for _, name := range keyEnvNames {
			if value, ok := env[name].(string); ok && strings.TrimSpace(value) != "" {
				return KeyResolution{Key: strings.TrimSpace(value), OK: true}
			}
		}
	}
	if IsLocalGatewayURL(baseURL) {
		return KeyResolution{Key: LocalGatewayKey, OK: true}
	}
	return KeyResolution{Detail: fmt.Sprintf("%s is absent from %s", strings.Join(keyEnvNames, "/"), file)}
}

// resolveKeyOrError resolves the credential for one call and returns the error
// the TS version throws when it is missing.
func resolveKeyOrError(baseURL string, lookup KeyLookup) (string, error) {
	resolution := ResolveAPIKey(baseURL, lookup)
	if resolution.OK {
		return resolution.Key, nil
	}
	detail := resolution.Detail
	if detail == "" {
		detail = "settings.json fallback unavailable"
	}
	return "", fmt.Errorf("missing %s: absent from the process environment; %s (fail-closed, no fallback)",
		strings.Join(keyEnvNames, "/"), detail)
}
