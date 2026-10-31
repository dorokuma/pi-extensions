//go:build !linux

package sandbox

func supported() (int, bool) {
	return 0, false
}

func applyPolicy(p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.BestEffort {
		p.warnf("sandbox: Landlock is only supported on Linux; degrading to no-op")
		return applyEnv(p.Env)
	}
	return ErrUnsupported
}
