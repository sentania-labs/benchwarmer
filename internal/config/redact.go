package config

import (
	"regexp"
	"strings"
)

// Redacted replaces secret values in reads, exports, and logs.
const Redacted = "<redacted>"

var secretFlag = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|credential)`)

// Redact returns a copy of c safe to return from the API or write to logs.
// Token values never live in the config (only file references), but runtime
// arguments can carry secrets such as llama-server's --api-key.
func Redact(c Config) Config {
	out := Clone(c)
	out.Runtime.Args = RedactArgs(out.Runtime.Args)
	return out
}

// RedactArgs masks values of secret-looking flags in an argument list, in
// both "--flag value" and "--flag=value" forms. Flags that name a file
// (ending in -file or -path) keep their value, since a path is not a secret.
func RedactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		a := out[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasEq := strings.Cut(a, "=")
		if !secretFlag.MatchString(name) || strings.HasSuffix(name, "-file") || strings.HasSuffix(name, "-path") {
			continue
		}
		if hasEq {
			if val != "" {
				out[i] = name + "=" + Redacted
			}
			continue
		}
		if i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			out[i+1] = Redacted
			i++
		}
	}
	return out
}

// RestoreRedacted merges secrets from old into an incoming config whose
// values were returned redacted, so a UI can round-trip a config it read.
// Values are matched by flag name, so adding, removing, or reordering other
// arguments does not break the round trip. A redacted value with no matching
// secret in old is left as the literal placeholder and fails validation.
func RestoreRedacted(incoming, old Config) Config {
	out := Clone(incoming)
	secrets := map[string]string{} // flag name -> original value
	oa := old.Runtime.Args
	for i := 0; i < len(oa); i++ {
		name, val, hasEq := strings.Cut(oa[i], "=")
		if !strings.HasPrefix(name, "-") {
			continue
		}
		if hasEq {
			secrets[name] = val
		} else if i+1 < len(oa) && !strings.HasPrefix(oa[i+1], "-") {
			secrets[name] = oa[i+1]
		}
	}
	a := out.Runtime.Args
	for i := range a {
		name, val, hasEq := strings.Cut(a[i], "=")
		switch {
		case hasEq && val == Redacted:
			if v, ok := secrets[name]; ok {
				a[i] = name + "=" + v
			}
		case a[i] == Redacted && i > 0:
			if v, ok := secrets[a[i-1]]; ok {
				a[i] = v
			}
		}
	}
	return out
}
