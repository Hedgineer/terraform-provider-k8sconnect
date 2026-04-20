package fieldmanagement

import "strings"

// Path encoding (ADR-025): Kubernetes map keys can contain '.' (e.g. ConfigMap data
// keys like "config.yaml", annotation keys like "app.kubernetes.io/name", ArgoCD
// config-map keys like "resource.customizations.ignoreDifferences.<group>_<kind>").
//
// We encode field paths with a quote-delimited syntax:
//
//   plain key:         spec.replicas
//   key with dots:     data."resource.customizations.admissionregistration.k8s.io_Config"
//   array selector:    spec.containers[name=nginx].image
//
// Rules:
//   * '.' inside "..." is part of the key, not a separator.
//   * '.' inside [...] is part of a selector value, not a separator.
//   * Inside "...", '\' escapes the next character (used for '\"' and '\\').
//   * A key is wrapped in "..." iff it contains '.', '"', or '\' — otherwise bare.
//
// Backslash escape is used only inside quotes. Outside quotes there is no escape;
// the quote wrapping itself provides the delimiter.

// EncodePathKey quotes and escapes a map key for embedding in a dotted path.
// Returns the key unchanged if it has no characters requiring encoding.
func EncodePathKey(key string) string {
	if !strings.ContainsAny(key, `."\`) {
		return key
	}
	var b strings.Builder
	b.Grow(len(key) + 2)
	b.WriteByte('"')
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

// DecodePathKey reverses EncodePathKey. If s is quote-wrapped, strips quotes and
// unescapes internal '\\' and '\"'. Otherwise returns s unchanged.
func DecodePathKey(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	inner := s[1 : len(s)-1]
	if !strings.Contains(inner, `\`) {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			b.WriteByte(inner[i+1])
			i++
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// JoinPath appends an already-encoded key segment to a prefix path with "."
// as separator. If prefix is empty, returns key.
func JoinPath(prefix, encodedKey string) string {
	if prefix == "" {
		return encodedKey
	}
	return prefix + "." + encodedKey
}

// SplitPath splits an encoded path into its raw (still-encoded) segments.
// Does not split on '.' inside "..." or [...].
func SplitPath(path string) []string {
	if path == "" {
		return nil
	}
	var segs []string
	var cur strings.Builder
	inQuote := false
	depth := 0
	for i := 0; i < len(path); i++ {
		c := path[i]
		if inQuote {
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(path) {
				cur.WriteByte(path[i+1])
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
			cur.WriteByte(c)
		case '[':
			depth++
			cur.WriteByte(c)
		case ']':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
		case '.':
			if depth == 0 {
				segs = append(segs, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, cur.String())
	return segs
}

// FindSelectorStart returns the index of the '[' that begins an array selector
// within a single raw segment, or -1 if none. '[' inside "..." does not count.
func FindSelectorStart(segment string) int {
	inQuote := false
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if inQuote {
			if c == '\\' && i+1 < len(segment) {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		if c == '"' {
			inQuote = true
			continue
		}
		if c == '[' {
			return i
		}
	}
	return -1
}
