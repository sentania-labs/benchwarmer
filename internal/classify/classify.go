// Package classify matches running processes against the configured
// application rules (config.AppRule). Matching is case-insensitive and treats
// / and \ alike; rules are evaluated in order and the first match wins.
package classify

import (
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/signals"
)

type rule struct {
	name, class string
	exe         string // normalized base name
	path        string // normalized full path
	prefix      string
	glob        string
}

// Classifier holds rules normalized once, so per-process matching allocates
// only for the process's own normalized path.
type Classifier struct {
	rules []rule
	src   []config.AppRule
}

// New builds a classifier from rules in evaluation order.
func New(rules []config.AppRule) *Classifier {
	c := &Classifier{src: append([]config.AppRule(nil), rules...)}
	for _, r := range rules {
		c.rules = append(c.rules, rule{
			name:   ruleName(r),
			class:  r.Class,
			exe:    norm(r.Exe),
			path:   norm(r.Path),
			prefix: norm(r.PathPrefix),
			glob:   norm(r.Glob),
		})
	}
	return c
}

// Rules returns the rules the classifier was built from.
func (c *Classifier) Rules() []config.AppRule { return c.src }

// Classify returns the class and rule name of the first rule matching p.
// When p.Path is empty (unreadable), only executable-name rules can match.
func (c *Classifier) Classify(p signals.Process) (class, rule string, matched bool) {
	return c.Match(p.Name, p.Path)
}

// Match classifies by executable name and full path, for callers that have
// no signals.Process (e.g. the session agent's foreground report). The base
// name is taken from path when present, otherwise from name.
func (c *Classifier) Match(name, path string) (class, rule string, matched bool) {
	np := norm(path)
	base := np
	if base == "" {
		base = norm(name)
	}
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	for _, r := range c.rules {
		var ok bool
		switch {
		case r.exe != "":
			ok = base != "" && base == r.exe
		case np == "":
			// Path rules cannot be evaluated without a path.
		case r.path != "":
			ok = np == r.path
		case r.prefix != "":
			ok = strings.HasPrefix(np, r.prefix)
		case r.glob != "":
			ok = globMatch(r.glob, np)
		}
		if ok {
			return r.class, r.name, true
		}
	}
	return "", "", false
}

func ruleName(r config.AppRule) string {
	if r.Name != "" {
		return r.Name
	}
	switch {
	case r.Exe != "":
		return "exe:" + r.Exe
	case r.Path != "":
		return "path:" + r.Path
	case r.PathPrefix != "":
		return "path_prefix:" + r.PathPrefix
	}
	return "glob:" + r.Glob
}

// norm lower-cases s and uses / as the only separator.
func norm(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
}

// globMatch reports whether s matches pattern, where * matches any run of
// characters including separators and ? matches exactly one character. Both
// inputs are already normalized. Iterative with single-star backtracking, so
// it is linear in practice and cannot blow up on hostile patterns.
func globMatch(pattern, s string) bool {
	p := []rune(pattern)
	r := []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(r) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == r[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
