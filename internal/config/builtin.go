package config

import "strings"

// Shipped application rules reach an existing config.json through
// mergeBuiltinRules: rules the file has not been offered yet are added, and
// rules the operator deleted are not brought back.

// builtinID names a shipped rule by its matcher, so ids need no upkeep when
// a rule is added and match what an older, id-less config already contains.
// The matcher is normalized the way internal/classify matches (lower case,
// / and \ alike), so a legacy rule written with the other separator is still
// recognised.
func builtinID(r AppRule) string {
	switch {
	case r.Exe != "":
		return "exe:" + normMatcher(r.Exe)
	case r.Path != "":
		return "path:" + normMatcher(r.Path)
	case r.PathPrefix != "":
		return "path_prefix:" + normMatcher(r.PathPrefix)
	case r.Glob != "":
		return "glob:" + normMatcher(r.Glob)
	}
	return ""
}

func normMatcher(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
}

// DefaultRuleIDs lists the ids of the shipped application rules.
func DefaultRuleIDs() []string {
	rules := DefaultApplications()
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return ids
}

// mergeBuiltinRules brings c.Applications up to the shipped list and
// reports whether it changed anything.
//
// A rule without an id (written before ids existed) whose matcher equals a
// shipped rule's is taken to be that rule, so an edited class survives; if
// the file already holds that id, or an earlier id-less rule took it, the
// rule stays local.
// Shipped rules not yet in BuiltinRulesSeen are inserted after the closest
// preceding shipped rule present, since the first match wins; a config that
// has never recorded what it was offered is treated as having seen only the
// shipped rules it already contains.
func mergeBuiltinRules(c *Config) bool {
	changed := false
	shipped := DefaultApplications()
	shippedIDs := map[string]bool{}
	for _, d := range shipped {
		shippedIDs[d.ID] = true
	}
	// Reserve every id already in the file before adopting any, so an
	// id-less rule that sits above the shipped rule it duplicates (a local
	// override) cannot take the id and collide with it.
	have := map[string]bool{}
	for _, r := range c.Applications {
		if r.ID != "" {
			have[r.ID] = true
		}
	}
	for i := range c.Applications {
		r := &c.Applications[i]
		if r.ID != "" {
			continue
		}
		if id := builtinID(*r); shippedIDs[id] && !have[id] {
			r.ID, changed = id, true
			have[id] = true
		}
	}
	seen := map[string]bool{}
	for _, id := range c.BuiltinRulesSeen {
		seen[id] = true
	}
	for i, d := range shipped {
		if have[d.ID] || seen[d.ID] {
			continue
		}
		at := 0
		for j := i - 1; j >= 0; j-- {
			if k := indexOfRule(c.Applications, shipped[j].ID); k >= 0 {
				at = k + 1
				break
			}
		}
		c.Applications = append(c.Applications, AppRule{})
		copy(c.Applications[at+1:], c.Applications[at:])
		c.Applications[at] = d
		have[d.ID], changed = true, true
	}
	for _, d := range shipped {
		if !seen[d.ID] {
			c.BuiltinRulesSeen = append(c.BuiltinRulesSeen, d.ID)
			changed = true
		}
	}
	return changed
}

func indexOfRule(rules []AppRule, id string) int {
	for i, r := range rules {
		if r.ID == id {
			return i
		}
	}
	return -1
}
