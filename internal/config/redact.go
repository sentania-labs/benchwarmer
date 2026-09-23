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
func RestoreRedacted(incoming, old Config) Config {
	out := Clone(incoming)
	oldArgs, oldRed := old.Runtime.Args, RedactArgs(old.Runtime.Args)
	if len(out.Runtime.Args) == len(oldArgs) {
		for i, a := range out.Runtime.Args {
			if strings.Contains(a, Redacted) && oldRed[i] == a {
				out.Runtime.Args[i] = oldArgs[i]
			}
		}
	}
	return out
}
