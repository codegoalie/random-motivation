package main

// selectionMode controls which suite of checks is eligible for a run.
type selectionMode uint8

const (
	// modeExisting targets an already-running service. Only checks that
	// are safe against arbitrary deployments are eligible.
	modeExisting selectionMode = iota
	// modeSelfManaged means the UAT command supervises the service
	// process and provides an isolated database, so the full suite is
	// eligible.
	modeSelfManaged
)

// selectMode derives the selection mode from CLI configuration.
// A non-empty start command implies the suite is self-managing the
// service; otherwise the suite targets an existing service.
func selectMode(cfg config) selectionMode {
	if cfg.startCommand != "" {
		return modeSelfManaged
	}
	return modeExisting
}

// selectChecks returns the subset of the provided checks that should
// run, preserving the declared order. Selection rules:
//
//   - destructive checks are excluded in existing-service mode and
//     whenever cfg.skipDestructive is true.
//   - render-required checks are excluded in existing-service mode
//     unless cfg.renderURL is set (the operator has explicitly pointed
//     the suite at a controllable render endpoint).
//   - self-managed mode includes everything by default.
func selectChecks(mode selectionMode, cfg config, checks []Check) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if !shouldInclude(mode, cfg, c.Kind) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// shouldInclude reports whether a check with the given kind bitmask
// should be included for the current mode and configuration.
func shouldInclude(mode selectionMode, cfg config, kind CheckKind) bool {
	isDestructive := kind&destructive != 0
	isRenderRequired := kind&renderRequired != 0

	if isDestructive {
		if cfg.skipDestructive {
			return false
		}
		if mode == modeExisting {
			return false
		}
	}

	if isRenderRequired && mode == modeExisting && cfg.renderURL == "" {
		return false
	}

	return true
}
