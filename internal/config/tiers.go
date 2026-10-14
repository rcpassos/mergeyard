package config

// TierSettings replaces both model and effort; empty fields use harness defaults.
type TierSettings struct {
	Model  string
	Effort string
}

func validTier(name string) bool {
	return name == "low" || name == "medium" || name == "high"
}

func readTiers(role mapping, target *map[string]map[string]TierSettings) {
	n := role.node("tiers")
	if n == nil {
		return
	}
	harnesses := newMapping(n, role.fieldPath("tiers"), role.err, role.warnings)
	*target = map[string]map[string]TierSettings{}
	for _, agent := range harnesses.keys {
		if agent != "claude" && agent != "codex" {
			harnesses.fail("config.invalid_agent", agent, "expected claude or codex")
			return
		}
		tiers := harnesses.child(agent)
		(*target)[agent] = map[string]TierSettings{}
		for _, name := range tiers.keys {
			if !validTier(name) {
				tiers.fail("config.invalid_tier", name, "expected low, medium, or high")
				return
			}
			entry := tiers.child(name)
			var settings TierSettings
			entry.readNullableString("model", &settings.Model)
			entry.readNullableString("effort", &settings.Effort)
			entry.finish()
			(*target)[agent][name] = settings
		}
		tiers.finish()
	}
	harnesses.finish()
}
