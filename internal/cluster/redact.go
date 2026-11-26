package cluster

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

const redacted = "*** redacted ***"

// minSecretLength keeps short values from redacting unrelated text.
const minSecretLength = 8

// sensitiveKey reports whether a secrets bundle field holds a secret: private keys, tokens and
// encryption secrets. Certificates and the cluster ID are public.
func sensitiveKey(key string) bool {
	key = strings.ToLower(key)

	return strings.Contains(key, "key") || strings.Contains(key, "token") || strings.Contains(key, "secret")
}

// collectSecrets walks the YAML form of the secrets bundle and returns every value stored under
// a sensitive field. The machine config embeds these values in the same encoding.
func collectSecrets(node any, sensitive bool, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			collectSecrets(child, sensitive || sensitiveKey(k), out)
		}
	case []any:
		for _, child := range v {
			collectSecrets(child, sensitive, out)
		}
	case string:
		if sensitive && len(v) >= minSecretLength {
			*out = append(*out, v)
			*out = append(*out, pemBodyLines(v)...)
		}
	}
}

// pemBodyLines returns the base64 lines of a base64-encoded PEM block. Talos v1.14 documents
// embed some keys as indented PEM text, which never matches the bundle value as a whole.
func pemBodyLines(v string) []string {
	decoded, err := base64.StdEncoding.DecodeString(v)
	if err != nil || !strings.Contains(string(decoded), "-----BEGIN") {
		return nil
	}

	var lines []string

	for _, l := range strings.Split(string(decoded), "\n") {
		l = strings.TrimSpace(l)
		if len(l) >= minSecretLength && !strings.HasPrefix(l, "-----") {
			lines = append(lines, l)
		}
	}

	return lines
}

// privateKeyBlock matches a PEM private key, also when indented or prefixed by diff markers.
var privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// redactor returns a function that replaces the cluster secrets in s with a marker, or nil when
// redaction is disabled or the secrets have not been loaded (nothing secret was rendered yet).
func (m *Manager) redactor() func(string) string {
	if !m.Options.Redact {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.secrets == nil {
		return nil
	}

	if m.redact == nil {
		data, err := yaml.Marshal(m.secrets)
		if err != nil {
			// Fail closed: without the secret list, hide nothing rather than print it.
			m.redact = func(string) string { return "*** output hidden: cannot determine secrets to redact ***\n" }

			return m.redact
		}

		var tree any
		if err := yaml.Unmarshal(data, &tree); err != nil {
			m.redact = func(string) string { return "*** output hidden: cannot determine secrets to redact ***\n" }

			return m.redact
		}

		var values []string
		collectSecrets(tree, false, &values)

		// Longest first, so a secret containing another one is replaced whole.
		sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })

		pairs := make([]string, 0, 2*len(values))
		for _, v := range values {
			pairs = append(pairs, v, redacted)
		}

		replacer := strings.NewReplacer(pairs...)

		// Any PEM private key is hidden too, even one that is not in the bundle.
		m.redact = func(s string) string {
			return privateKeyBlock.ReplaceAllString(replacer.Replace(s), redacted)
		}
	}

	return m.redact
}
