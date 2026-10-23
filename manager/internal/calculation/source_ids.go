package calculation

// RemapRoutingIDs rewrites only configured state-input routing. Rule names,
// expressions, ports and station addresses are never inferred or changed.
// As with ReplaceStateInputs, TOML layout/comments are normalized on change.
func RemapRoutingIDs(content string, sources, groups map[string]string) (string, bool, error) {
	config, err := ParseConfig(content)
	if err != nil {
		return content, false, err
	}
	changed := false
	for i := range config.StateInputs {
		if id, ok := groups[config.StateInputs[i].GroupID]; ok && id != config.StateInputs[i].GroupID {
			config.StateInputs[i].GroupID, changed = id, true
		}
		for j := range config.StateInputs[i].Targets {
			target := &config.StateInputs[i].Targets[j]
			if id, ok := sources[target.SourceID]; ok && id != target.SourceID {
				target.SourceID, changed = id, true
			}
		}
	}
	if !changed {
		return content, false, nil
	}
	updated, err := ReplaceStateInputs(content, config.StateInputs)
	return updated, true, err
}
