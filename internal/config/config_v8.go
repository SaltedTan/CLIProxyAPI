package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Config remains the effective runtime/wire representation. Layout translation is
// confined to YAML boundaries so older SDK and Home consumers keep their contract.
type legacyConfig Config

type configPath struct{ old, current string }

var v8FieldIndexes = make(map[string][]int)
var v8StructPaths []configPath
var v8Paths = buildV8Paths()

// Historical spellings are YAML-boundary aliases, not provider runtime fields.
// The canonical client path wins by presence (including explicit false/null).
// If it is absent, prefer the previous v8 OAuth path, then providers, then codex.
var v8ClientPaths = []configPath{
	{"oauth.providers.codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
	{"providers.codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
	{"codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
}

// Canonical upstream fields win by presence; historical OAuth fields precede globals.
var v8SharedPaths = []configPath{
	{"oauth.providers.codex.disable-codex-cloaking", "upstream.codex.disable-codex-cloaking"},
	{"oauth.providers.codex.stream-bootstrap-buffering", "upstream.codex.stream-bootstrap-buffering"},
	{"oauth.providers.codex.stream-bootstrap-timeout", "upstream.codex.stream-bootstrap-timeout"},
	{"oauth.providers.codex.orphan-delegation-compatibility", "upstream.codex.orphan-delegation-compatibility"},
	{"oauth.providers.codex.model-level-cooling", "upstream.codex.model-level-cooling"},
	{"oauth.providers.codex.response-steering", "upstream.codex.response-steering"},
	{"oauth.providers.claude.model-level-cooling", "upstream.claude.model-level-cooling"},
	{"oauth.providers.claude.claude-code.disable-cloaking-model-list", "upstream.claude.disable-cloaking-model-list"},
	{"oauth.providers.claude.disable-claude-cloak-mode", "upstream.claude.disable-claude-cloak-mode"},
	{"oauth.providers.claude.header-defaults.user-agent", "upstream.claude.header-defaults.user-agent"},
	{"oauth.providers.claude.header-defaults.package-version", "upstream.claude.header-defaults.package-version"},
	{"oauth.providers.claude.header-defaults.runtime-version", "upstream.claude.header-defaults.runtime-version"},
	{"oauth.providers.claude.header-defaults.os", "upstream.claude.header-defaults.os"},
	{"oauth.providers.claude.header-defaults.arch", "upstream.claude.header-defaults.arch"},
	{"oauth.providers.claude.header-defaults.timeout", "upstream.claude.header-defaults.timeout"},
	{"oauth.providers.claude.header-defaults.timezone", "upstream.claude.header-defaults.timezone"},
	{"oauth.providers.claude.header-defaults.stabilize-device-profile", "upstream.claude.header-defaults.stabilize-device-profile"},
	{"oauth.providers.xai.inject-x-search", "upstream.xai.inject-x-search"},
}

var v8Aliases = append(append([]configPath(nil), v8ClientPaths...), v8SharedPaths...)

var v8SharedStructPaths = []configPath{
	{"oauth.providers.claude.header-defaults", "upstream.claude.header-defaults"},
	{"oauth.providers.claude.claude-code", "upstream.claude"},
	{"oauth.providers.claude", "upstream.claude"},
	{"oauth.providers.xai", "upstream.xai"},
}

var v8KeyFamilies = []configPath{
	{"gemini-api-key", "gemini"}, {"interactions-api-key", "interactions"},
	{"vertex-api-key", "vertex"}, {"codex-api-key", "codex"},
	{"claude-api-key", "claude"}, {"xai-api-key", "xai"}, {"meta-api-key", "meta"},
	{"openai-compatibility", "openai-compatibility"},
}

func buildV8Paths() []configPath {
	prefixes := []configPath{
		{"host", "server.host"}, {"port", "server.port"}, {"trusted-proxies", "server.trusted-proxies"},
		{"tls", "server.tls"}, {"commercial-mode", "server.commercial-mode"}, {"discovery", "server.discovery"},
		{"remote-management", "management"}, {"api-keys", "access.api-keys"}, {"api-key-names", "access.api-key-names"},
		{"api-key-limits", "access.api-key-limits"},
		{"credential-concurrency", "credentials.concurrency"}, {"credential-in-flight", "credentials.in-flight"},
		{"force-model-prefix", "routing.force-model-prefix"},
		{"request-retry", "routing.retry.request-retry"}, {"max-retry-credentials", "routing.retry.max-retry-credentials"},
		{"max-retry-interval", "routing.retry.max-retry-interval"},
		{"disable-cooling", "routing.cooldown.disable-cooling"}, {"save-cooldown-status", "routing.cooldown.save-cooldown-status"},
		{"transient-error-cooldown-seconds", "routing.cooldown.transient-error-cooldown-seconds"},
		{"proxy-url", "requests.proxy-url"}, {"passthrough-headers", "requests.passthrough-headers"},
		{"nonstream-keepalive-interval", "requests.nonstream-keepalive-interval"}, {"streaming", "requests.streaming"}, {"payload", "requests.payload"},
		{"auth-dir", "oauth.auth-dir"}, {"auth-auto-refresh-workers", "oauth.auth-auto-refresh-workers"},
		{"oauth-model-alias", "oauth.model-alias"}, {"oauth-excluded-models", "oauth.excluded-models"},
		{"oauth-request-scoped-errors", "oauth.request-scoped-errors"}, {"oauth-settings", "oauth.settings"}, {"ws-auth", "oauth.providers.aistudio.ws-auth"},
		{"codex.disable-codex-cloaking", "upstream.codex.disable-codex-cloaking"},
		{"codex.stream-bootstrap-buffering", "upstream.codex.stream-bootstrap-buffering"},
		{"codex.stream-bootstrap-timeout", "upstream.codex.stream-bootstrap-timeout"},
		{"codex.orphan-delegation-compatibility", "upstream.codex.orphan-delegation-compatibility"},
		{"codex.model-level-cooling", "upstream.codex.model-level-cooling"},
		{"codex.response-steering", "upstream.codex.response-steering"},
		{"codex", "oauth.providers.codex"}, {"codex-header-defaults", "oauth.providers.codex.header-defaults"},
		{"claude", "upstream.claude"}, {"claude-code", "upstream.claude"},
		{"disable-claude-cloak-mode", "upstream.claude.disable-claude-cloak-mode"},
		{"claude-header-defaults", "upstream.claude.header-defaults"},
		{"antigravity", "oauth.providers.antigravity"},
		{"antigravity-signature-cache-enabled", "oauth.providers.antigravity.signature-cache-enabled"},
		{"antigravity-signature-bypass-strict", "oauth.providers.antigravity.signature-bypass-strict"},
		{"quota-exceeded.antigravity-credits", "oauth.providers.antigravity.antigravity-credits"},
		{"xai", "upstream.xai"}, {"devin", "oauth.providers.devin"},
		{"disable-image-generation", "multimedia.disable-image-generation"}, {"gpt-image-2-base-model", "multimedia.gpt-image-2-base-model"},
		{"video-result-auth-cache-ttl", "multimedia.video-result-auth-cache-ttl"},
		{"debug", "observability.logs.debug"}, {"logging-to-file", "observability.logs.logging-to-file"},
		{"logs-max-total-size-mb", "observability.logs.logs-max-total-size-mb"}, {"request-log", "observability.logs.request-log"},
		{"error-logs-max-files", "observability.logs.error-logs-max-files"},
		{"usage-statistics-enabled", "observability.usage.usage-statistics-enabled"},
		{"redis-usage-queue-retention-seconds", "observability.usage.redis-usage-queue-retention-seconds"}, {"pprof", "observability.pprof"},
	}
	var out []configPath
	var walk func(reflect.Type, string, []int)
	walk = func(t reflect.Type, path string, indexes []int) {
		if t.Kind() == reflect.Struct {
			for _, prefix := range prefixes {
				if path == prefix.old || strings.HasPrefix(path, prefix.old+".") {
					v8StructPaths = append(v8StructPaths, configPath{path, prefix.current + strings.TrimPrefix(path, prefix.old)})
					break
				}
			}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
				if tag == "-" || field.PkgPath != "" {
					continue
				}
				fieldIndexes := append(append([]int(nil), indexes...), i)
				if field.Anonymous {
					walk(field.Type, path, fieldIndexes)
					continue
				}
				child := tag
				if path != "" {
					child = path + "." + tag
				}
				walk(field.Type, child, fieldIndexes)
			}
			return
		}
		for _, prefix := range prefixes {
			if path == prefix.old || strings.HasPrefix(path, prefix.old+".") {
				out = append(out, configPath{path, prefix.current + strings.TrimPrefix(path, prefix.old)})
				v8FieldIndexes[path] = indexes
				return
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "", nil)
	return out
}

// clientKeyMapNames are the config maps keyed by raw client API keys. Their
// entries must never appear unmasked in errors or logs.
var clientKeyMapNames = map[string]struct{}{"api-key-limits": {}, "api-key-names": {}}

// decoderKeyPatterns match the YAML decoder errors that print a mapping key or an
// anchor name: a duplicated key, a key that is not a scalar, and an anchor that
// contains itself.
var (
	duplicateMappingKeyPattern = regexp.MustCompile(`mapping key ("(?:[^"\\]|\\.)*") already defined`)
	invalidMapKeyPattern       = regexp.MustCompile(`invalid map key: [^\n]*`)
	selfAnchorPattern          = regexp.MustCompile(`anchor '([^']*)' value contains itself`)
	unknownAnchorPattern       = regexp.MustCompile(`unknown anchor '([^']*)' referenced`)
	// The scalar may itself contain backticks or newlines: capture up to the last one.
	taggedScalarPattern = regexp.MustCompile("(?s)cannot decode (\\S+) `(.*)` as a")
	// A value that does not convert to its destination type is printed by the
	// decoder (truncated to seven characters). It may be a client key aliased
	// into any typed field; several such lines may follow each other.
	unmarshalTargetPattern = regexp.MustCompile("(?sm)cannot unmarshal (\\S+) `(.*?)` into ([^`\\n]+)$")
	// The strict decoder names a field its destination type does not have.
	unknownFieldPattern = regexp.MustCompile("(?s)field (.*?) not found in type ")
)

// clientKeySet holds the client keys a document names in its client key maps
// and client key lists. The operator can alias such a key elsewhere, where it
// becomes a section, provider or field name; diagnostics naming one mask it.
type clientKeySet map[string]struct{}

// mask returns name masked when it is one of the client keys, unchanged otherwise.
func (keys clientKeySet) mask(name string) string {
	if _, ok := keys[name]; ok {
		return maskClientKey(name)
	}
	return name
}

// maskUnknownFields masks the client keys the strict decoder names as unknown fields.
func (keys clientKeySet) maskUnknownFields(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	masked := unknownFieldPattern.ReplaceAllStringFunc(message, func(match string) string {
		return "field " + keys.mask(unknownFieldPattern.FindStringSubmatch(match)[1]) + " not found in type "
	})
	if masked == message {
		return err
	}
	return errors.New(masked)
}

// maskText masks every occurrence of a client key in text that is not part of a
// longer word, ignoring case, as written, trimmed or escaped by %q: validators
// print an offending value, sometimes normalized, and that value can be a client
// key the operator aliased into their field.
func (keys clientKeySet) maskText(text string) string {
	seen := make(map[string]struct{})
	var forms []string
	for key := range keys {
		for _, form := range []string{key, strings.TrimSpace(key)} {
			quoted := strconv.Quote(form)
			for _, form := range []string{form, quoted[1 : len(quoted)-1]} {
				if _, dup := seen[form]; form != "" && !dup {
					seen[form] = struct{}{}
					forms = append(forms, regexp.QuoteMeta(form))
				}
			}
		}
	}
	if len(forms) == 0 {
		return text
	}
	// The longest form wins where several start at the same position.
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	pattern, err := regexp.Compile("(?i)" + strings.Join(forms, "|"))
	if err != nil {
		return text
	}
	var out strings.Builder
	last := 0
	for _, match := range pattern.FindAllStringIndex(text, -1) {
		if !standsAlone(text, match[0], match[1]) {
			continue
		}
		out.WriteString(text[last:match[0]])
		out.WriteString(maskClientKey(text[match[0]:match[1]]))
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

// standsAlone reports whether text[start:end] does not continue a word on
// either side, so that a short key does not mask part of an unrelated word.
func standsAlone(text string, start, end int) bool {
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	first, _ := utf8.DecodeRuneInString(text[start:end])
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	final, _ := utf8.DecodeLastRuneInString(text[start:end])
	after, _ := utf8.DecodeRuneInString(text[end:])
	return !(isWord(first) && isWord(before)) && !(isWord(final) && isWord(after))
}

// maskClientKeysIn masks in err every client key the document data names. The
// public entry points apply it last, to every diagnostic they return.
func maskClientKeysIn(data []byte, err error) error {
	if err == nil {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return err
	}
	// Keys collected before a rejected entry are still masked.
	keys, _ := clientKeysOf(doc.Content[0])
	message := err.Error()
	masked := keys.maskText(message)
	if masked == message {
		return err
	}
	return errors.New(masked)
}

// validateClientKeyMaps rejects a client key map (api-key-limits, api-key-names)
// that is not a mapping, and any duplicated, non-plain or, for an allowance,
// non-numeric entry in one, wherever the document holds the map: directly, via
// a merge key, behind an alias or anchored elsewhere. It runs before any YAML
// decoding so that no decoder message can print a client key, and names only
// the map and the line. It also keeps an empty flow-style value an explicit
// null so that re-encoding the document preserves "no limit".
func validateClientKeyMaps(root *yaml.Node) error {
	_, err := clientKeysOf(root)
	return err
}

// clientKeysOf runs the checks of validateClientKeyMaps and returns the client
// keys the document names: the entries of its client key maps and lists.
func clientKeysOf(root *yaml.Node) (clientKeySet, error) {
	keys := make(clientKeySet)
	err := walkClientKeyMaps(root, make(map[*yaml.Node]struct{}), keys)
	return keys, err
}

func walkClientKeyMaps(node *yaml.Node, seen map[*yaml.Node]struct{}, keys clientKeySet) error {
	node = resolveAliasNode(node, seen)
	if node == nil {
		return nil
	}
	if _, visited := seen[node]; visited {
		return nil
	}
	seen[node] = struct{}{}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := walkClientKeyMaps(child, seen, keys); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			name := mapKeyName(node.Content[i])
			if _, clientKeyMap := clientKeyMapNames[name]; clientKeyMap {
				// Entries of a client key map are client keys whatever their
				// text, never configuration fields: the walk checks them and
				// does not descend into them.
				if err := checkClientKeyMapEntries(node, node.Content[i+1], name, keys); err != nil {
					return err
				}
				continue
			}
			if name == "api-keys" {
				collectClientKeyList(node.Content[i+1], keys)
			}
			if err := walkClientKeyMaps(node.Content[i+1], seen, keys); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectClientKeyList adds the entries of a client key list to keys. Only the
// list form of api-keys holds client keys; the v8 map of that name holds
// upstream credentials.
func collectClientKeyList(list *yaml.Node, keys clientKeySet) {
	list = resolveAliasNode(list, make(map[*yaml.Node]struct{}))
	if list == nil || list.Kind != yaml.SequenceNode {
		return
	}
	for _, entry := range list.Content {
		if scalar := scalarKeyNode(entry); scalar != nil {
			keys[scalar.Value] = struct{}{}
		}
	}
}

// scalarKey returns the scalar a mapping key decodes to, following aliases, and
// whether the key is a scalar at all.
func scalarKey(key *yaml.Node) (string, bool) {
	resolved := scalarKeyNode(key)
	if resolved == nil {
		return "", false
	}
	return resolved.Value, true
}

// scalarKeyNode returns the scalar node a mapping key resolves to, or nil.
func scalarKeyNode(key *yaml.Node) *yaml.Node {
	resolved := resolveAliasNode(key, make(map[*yaml.Node]struct{}))
	if resolved == nil || resolved.Kind != yaml.ScalarNode {
		return nil
	}
	return resolved
}

// mapKeyName returns the name a mapping key decodes to: the merge marker for <<,
// the resolved scalar otherwise, and an empty name for a key that is not a scalar.
func mapKeyName(key *yaml.Node) string {
	if isMergeKey(key) {
		return "<<"
	}
	name, _ := scalarKey(key)
	return name
}

// checkClientKeyMapEntries reports the first entry of the named client key map
// that is not a plain scalar key (a compound or explicitly tagged key, which the
// decoder would otherwise print), is defined twice within one of its mappings,
// or has a value the decoder would reject, printing it: values must be plain
// scalars, and allowances numbers or empty. Alias keys are compared by the
// scalar they resolve to. Each entry's key is added to keys.
func checkClientKeyMapEntries(parent, mapping *yaml.Node, name string, keys clientKeySet) error {
	// The map itself must be a mapping or empty: the decoder would otherwise
	// print a scalar found there, which may well be a client key.
	container := resolveAliasNode(mapping, make(map[*yaml.Node]struct{}))
	if container != nil && container.Kind != yaml.MappingNode && !isNullScalar(container) {
		return fmt.Errorf("%s: value at line %d must be a mapping", name, mapping.Line)
	}
	keepNullExplicit(parent, container)
	for _, node := range mergedMappings(mapping, nil, make(map[*yaml.Node]struct{})) {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			if isMergeKey(node.Content[i]) {
				continue
			}
			line := node.Content[i].Line
			resolved := scalarKeyNode(node.Content[i])
			if resolved == nil || resolved.Style&yaml.TaggedStyle != 0 {
				return fmt.Errorf("%s: entry at line %d must be a plain key", name, line)
			}
			key := resolved.Value
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s: entry %s is defined more than once (line %d)", name, maskClientKey(key), line)
			}
			seen[key] = struct{}{}
			keys[key] = struct{}{}
			value := scalarKeyNode(node.Content[i+1])
			if value == nil || value.Style&yaml.TaggedStyle != 0 {
				return fmt.Errorf("%s: entry at line %d must have a plain scalar value", name, line)
			}
			if name == "api-key-limits" && value.Tag != "!!int" && value.Tag != "!!float" && value.Tag != "!!null" {
				return fmt.Errorf("%s: entry at line %d must have a numeric value", name, line)
			}
			keepNullExplicit(node, value)
		}
	}
	return nil
}

// maskDecoderError masks the key, value or anchor named by the YAML parser's and
// decoder's own errors: a duplicated or non-scalar key, an explicitly tagged
// scalar its tag does not fit, and an unknown or self-referencing anchor. It is
// the fallback for an entry the structural check could not attribute to a client
// key map, such as an anchor declared under an unrelated name; line numbers are
// kept so the entry stays findable.
func maskDecoderError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	masked := duplicateMappingKeyPattern.ReplaceAllStringFunc(message, func(match string) string {
		quoted := duplicateMappingKeyPattern.FindStringSubmatch(match)[1]
		key, errUnquote := strconv.Unquote(quoted)
		if errUnquote != nil {
			key = quoted
		}
		return "mapping key " + strconv.Quote(maskClientKey(key)) + " already defined"
	})
	masked = invalidMapKeyPattern.ReplaceAllLiteralString(masked, "invalid map key: a key must be a plain scalar")
	masked = selfAnchorPattern.ReplaceAllStringFunc(masked, func(match string) string {
		return "anchor '" + maskClientKey(selfAnchorPattern.FindStringSubmatch(match)[1]) + "' value contains itself"
	})
	masked = unknownAnchorPattern.ReplaceAllStringFunc(masked, func(match string) string {
		return "unknown anchor '" + maskClientKey(unknownAnchorPattern.FindStringSubmatch(match)[1]) + "' referenced"
	})
	masked = taggedScalarPattern.ReplaceAllStringFunc(masked, func(match string) string {
		parts := taggedScalarPattern.FindStringSubmatch(match)
		return "cannot decode " + parts[1] + " `" + maskClientKey(parts[2]) + "` as a"
	})
	masked = unmarshalTargetPattern.ReplaceAllStringFunc(masked, func(match string) string {
		parts := unmarshalTargetPattern.FindStringSubmatch(match)
		return "cannot unmarshal " + parts[1] + " `" + maskClientKey(parts[2]) + "` into " + parts[3]
	})
	if masked == message {
		return err
	}
	return errors.New(masked)
}

// keepNullExplicit gives an empty value in a flow mapping its explicit null:
// yaml.v3 re-encodes an empty flow value as ” (an empty string), which would
// turn "no limit" into an invalid allowance on the next save or validation.
func keepNullExplicit(mapping, value *yaml.Node) {
	if mapping == nil || value == nil || mapping.Style&yaml.FlowStyle == 0 {
		return
	}
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" && value.Value == "" {
		value.Value = "null"
	}
}

// isNullScalar reports whether node is YAML null, including an empty value.
func isNullScalar(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode {
		return false
	}
	return node.Tag == "!!null" || (node.Tag == "" && node.Value == "")
}

// isMergeKey reports whether key is a YAML merge key (<<).
func isMergeKey(key *yaml.Node) bool {
	return key != nil && key.Kind == yaml.ScalarNode && key.Value == "<<" && (key.Tag == "" || key.Tag == "!" || key.Tag == "!!merge")
}

// resolveAliasNode follows aliases to the anchored node. It returns nil for a
// node already seen, which only happens when an anchor contains its own alias.
func resolveAliasNode(node *yaml.Node, seen map[*yaml.Node]struct{}) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		if _, cyclic := seen[node]; cyclic {
			return nil
		}
		seen[node] = struct{}{}
		node = node.Alias
	}
	return node
}

// mergeSources lists the mappings merged into node by its merge keys, in the
// order yaml.v3 applies them: earlier sources win over later ones.
func mergeSources(node *yaml.Node, seen map[*yaml.Node]struct{}) []*yaml.Node {
	var sources []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		if !isMergeKey(node.Content[i]) {
			continue
		}
		merge := resolveAliasNode(node.Content[i+1], seen)
		if merge == nil {
			continue
		}
		candidates := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			candidates = merge.Content
		}
		for _, candidate := range candidates {
			if source := resolveAliasNode(candidate, seen); source != nil && source.Kind == yaml.MappingNode {
				sources = append(sources, source)
			}
		}
	}
	return sources
}

// mergedMappings returns node and every mapping merged into it, recursively.
func mergedMappings(node *yaml.Node, out []*yaml.Node, seen map[*yaml.Node]struct{}) []*yaml.Node {
	node = resolveAliasNode(node, seen)
	if node == nil || node.Kind != yaml.MappingNode {
		return out
	}
	if _, visited := seen[node]; visited {
		return out
	}
	seen[node] = struct{}{}
	out = append(out, node)
	for _, source := range mergeSources(node, seen) {
		out = mergedMappings(source, out, seen)
	}
	return out
}

func yamlPath(root *yaml.Node, path string) *yaml.Node {
	for _, key := range strings.Split(path, ".") {
		idx := findMapKeyIndex(root, key)
		if idx < 0 {
			return nil
		}
		root = root.Content[idx+1]
	}
	return root
}

func setYAMLPath(root *yaml.Node, path string, value *yaml.Node) {
	parts := strings.Split(path, ".")
	for _, key := range parts[:len(parts)-1] {
		root = getOrCreateMapValue(root, key)
	}
	dst := getOrCreateMapValue(root, parts[len(parts)-1])
	*dst = *deepCopyNode(value)
}

// setYAMLPathWithComments attaches leading comments to the destination key;
// yaml.v3 does not render a scalar value's HeadComment.
func setYAMLPathWithComments(root *yaml.Node, path string, value *yaml.Node) {
	setYAMLPath(root, path, value)
	parts := strings.Split(path, ".")
	parent := root
	if len(parts) > 1 {
		parent = yamlPath(root, strings.Join(parts[:len(parts)-1], "."))
	}
	parent.Content[findMapKeyIndex(parent, parts[len(parts)-1])].HeadComment = value.HeadComment
	parent.Content[findMapKeyIndex(parent, parts[len(parts)-1])].FootComment = value.FootComment
	yamlPath(root, path).HeadComment = ""
	yamlPath(root, path).FootComment = ""
}

// copyYAMLPathValue carries key comments and comments of pruned ancestors.
func copyYAMLPathValue(root *yaml.Node, path string) *yaml.Node {
	copy := deepCopyNode(yamlPath(root, path))
	parts := strings.Split(path, ".")
	parent := root
	parents := []*yaml.Node{root}
	if len(parts) > 1 {
		for _, part := range parts[:len(parts)-1] {
			parent = yamlPath(parent, part)
			parents = append(parents, parent)
		}
	}
	if idx := findMapKeyIndex(parent, parts[len(parts)-1]); idx >= 0 {
		key := parent.Content[idx]
		copy.HeadComment = strings.TrimSpace(key.HeadComment + "\n" + key.LineComment + "\n" + copy.HeadComment)
		copy.FootComment = strings.TrimSpace(copy.FootComment + "\n" + key.FootComment)
	}
	// A move also prunes single-child ancestors. Carry their comments with the
	// field; section line comments become leading comments at the destination.
	for i := len(parents) - 1; i > 0 && len(parents[i].Content) == 2; i-- {
		idx := findMapKeyIndex(parents[i-1], parts[i-1])
		key := parents[i-1].Content[idx]
		copy.HeadComment = strings.TrimSpace(key.HeadComment + "\n" + key.LineComment + "\n" + parents[i].HeadComment + "\n" + parents[i].LineComment + "\n" + copy.HeadComment)
		copy.FootComment = strings.TrimSpace(copy.FootComment + "\n" + parents[i].FootComment + "\n" + key.FootComment)
	}
	return copy
}

func deleteYAMLPath(root *yaml.Node, path string) bool {
	parts := strings.SplitN(path, ".", 2)
	idx := findMapKeyIndex(root, parts[0])
	if idx < 0 {
		return false
	}
	if len(parts) == 2 {
		child := root.Content[idx+1]
		if !deleteYAMLPath(child, parts[1]) {
			return false
		}
		if len(child.Content) != 0 {
			return true
		}
	}
	root.Content = append(root.Content[:idx], root.Content[idx+2:]...)
	return true
}

func legacyPath(root *yaml.Node, path string) *yaml.Node {
	node := yamlPath(root, path)
	if path == "api-keys" && node != nil && node.Kind == yaml.MappingNode {
		return nil
	}
	return node
}

// UnmarshalYAML accepts both layouts, including partially migrated documents.
// Presence, rather than Go zero values, determines which setting wins.
func (cfg *Config) UnmarshalYAML(node *yaml.Node) error {
	root, err := flattenV8(node)
	if err != nil {
		return err
	}
	decoded := legacyConfig(*cfg)
	if err = root.Decode(&decoded); err != nil {
		// Callers decoding straight into Config see the typed decoder's message.
		return maskDecoderError(err)
	}
	*cfg = Config(decoded)
	cfg.OAuthOnlyFields = nil
	source := expandConfigAliases(node)
	for _, path := range v8Paths {
		if strings.HasPrefix(path.current, "oauth.providers.") && yamlPath(source, path.current) != nil {
			if cfg.OAuthOnlyFields == nil {
				cfg.OAuthOnlyFields = make(map[string]bool)
			}
			cfg.OAuthOnlyFields[path.old] = true
		}
	}
	return nil
}

func flattenV8(node *yaml.Node) (*yaml.Node, error) {
	root, _, err := flattenV8WithClientKeys(node)
	return root, err
}

// flattenV8WithClientKeys is flattenV8 that also returns the client keys the
// document names, which callers mask in their own diagnostics.
func flattenV8WithClientKeys(node *yaml.Node) (*yaml.Node, clientKeySet, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config must be a mapping")
	}
	// Maps keyed by client API keys are checked first so a duplicate is reported
	// masked; the generic decoder below would name the key in its error.
	keys, err := clientKeysOf(node)
	if err != nil {
		return nil, nil, err
	}
	// Decode once before transformation to reject duplicate keys even when a
	// winning v8 value would otherwise hide the malformed legacy subtree.
	var shape map[string]any
	if err := node.Decode(&shape); err != nil {
		return nil, nil, maskDecoderError(err)
	}
	node = expandConfigAliases(node)
	if _, err := normalizeV8PrivateIPAlias(node, true); err != nil {
		return nil, nil, err
	}
	for _, path := range append(append([]configPath(nil), v8Paths...), v8Aliases...) {
		parts := strings.Split(path.current, ".")
		for i := 1; i < len(parts); i++ {
			parent := yamlPath(node, strings.Join(parts[:i], "."))
			if parent == nil {
				break
			}
			// Routing is shared with the legacy layout, where null means defaults.
			if i == 1 && parts[0] == "routing" && parent.Tag == "!!null" {
				break
			}
			if parent.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("%s must be a mapping", strings.Join(parts[:i], "."))
			}
		}
	}
	root := deepCopyNode(node)
	for _, path := range v8Aliases {
		if yamlPath(root, path.old) != nil {
			if yamlPath(root, path.current) == nil {
				setYAMLPathWithComments(root, path.current, copyYAMLPathValue(root, path.old))
			}
			deleteYAMLPath(root, path.old)
		}
	}
	for _, path := range v8SharedStructPaths {
		value := yamlPath(root, path.old)
		if value == nil {
			continue
		}
		if value.Tag != "!!null" && value.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("%s must be a mapping", path.old)
		}
		if value.Tag != "!!null" && len(value.Content) != 0 {
			continue
		}
		if yamlPath(root, path.current) == nil {
			copy := copyYAMLPathValue(root, path.old)
			copy.Kind, copy.Tag, copy.Value = yaml.MappingNode, "!!map", ""
			setYAMLPathWithComments(root, path.current, copy)
		}
		deleteYAMLPath(root, path.old)
	}
	if version := yamlPath(root, "config-version"); version != nil && (version.Tag != "!!int" || version.Value != "8") {
		return nil, nil, fmt.Errorf("unsupported config-version (expected 8)")
	}
	// The v8 upstream map reuses the legacy client-key field name.
	if keys := yamlPath(root, "api-keys"); keys != nil && keys.Kind == yaml.MappingNode {
		deleteYAMLPath(root, "api-keys")
	}
	for _, path := range v8Paths {
		if value := yamlPath(root, path.current); value != nil {
			copy := copyYAMLPathValue(root, path.current)
			deleteYAMLPath(root, path.current)
			setYAMLPathWithComments(root, path.old, copy)
		}
	}
	for _, family := range v8KeyFamilies {
		if groups := yamlPath(node, "api-keys."+family.current); groups != nil {
			entries, err := expandV8Groups(groups, family.current, keys)
			if err != nil {
				return nil, nil, err
			}
			setYAMLPath(root, family.old, entries)
		}
	}
	return root, keys, nil
}

var sharedKeyFields = map[string]bool{
	"priority": true, "prefix": true, "proxy-url": true, "headers": true,
	"models": true, "excluded-models": true, "disable-cooling": true,
	"request-retry": true, "request-scoped-errors": true,
}

func expandV8Groups(groups *yaml.Node, provider string, clientKeys clientKeySet) (*yaml.Node, error) {
	if groups.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("api-keys.%s must be a list", provider)
	}
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, group := range groups.Content {
		if group.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("api-keys.%s[%d] must be a mapping", provider, index)
		}
		keys := yamlPath(group, "keys")
		if keys == nil || keys.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("api-keys.%s[%d].keys must be a list", provider, index)
		}
		if err := validateWeightSequenceNode(keys, "api-keys."+provider+".keys"); err != nil {
			return nil, err
		}
		if provider == "openai-compatibility" {
			item := deepCopyNode(group)
			deleteYAMLPath(item, "keys")
			deleteYAMLPath(item, "auth_index")
			deleteYAMLPath(item, "auth-index")
			cleanKeys := deepCopyNode(keys)
			for _, k := range cleanKeys.Content {
				deleteYAMLPath(k, "auth_index")
				deleteYAMLPath(k, "auth-index")
			}
			setYAMLPath(item, "api-key-entries", cleanKeys)
			out.Content = append(out.Content, item)
			continue
		}
		for i := 0; i < len(group.Content); i += 2 {
			field := group.Content[i].Value
			if field != "name" && field != "base-url" && field != "keys" && !sharedKeyFields[field] {
				return nil, fmt.Errorf("api-keys.%s: unsupported group field %s", provider, clientKeys.mask(field))
			}
		}
		for _, key := range keys.Content {
			if key.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("api-keys.%s key must be a mapping", provider)
			}
			if yamlPath(key, "base-url") != nil {
				return nil, fmt.Errorf("api-keys.%s: base-url belongs to the group", provider)
			}
			item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for i := 0; i < len(group.Content); i += 2 {
				field := group.Content[i].Value
				if field == "base-url" || sharedKeyFields[field] {
					setYAMLPath(item, field, group.Content[i+1])
				}
			}
			for i := 0; i < len(key.Content); i += 2 {
				if key.Content[i].Value == "auth_index" || key.Content[i].Value == "auth-index" {
					continue
				}
				if key.Content[i+1].Tag != "!!null" {
					setYAMLPath(item, key.Content[i].Value, key.Content[i+1])
				}
			}
			out.Content = append(out.Content, item)
		}
	}
	return out, nil
}

// groupLegacyKeys deliberately keeps one group per legacy entry: equal endpoints
// do not imply equal routing, headers, models, or credential policies.
func groupLegacyKeys(keys *yaml.Node, provider string) *yaml.Node {
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, entry := range keys.Content {
		group := deepCopyNode(entry)
		if provider == "openai-compatibility" {
			entries := yamlPath(group, "api-key-entries")
			if entries == nil {
				entries = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			}
			setYAMLPath(group, "keys", entries)
			deleteYAMLPath(group, "api-key-entries")
		} else {
			group = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setYAMLPath(group, "name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%s-%d", provider, index+1)})
			key := deepCopyNode(entry)
			for i := 0; i < len(entry.Content); i += 2 {
				field := entry.Content[i].Value
				if field == "base-url" || sharedKeyFields[field] {
					setYAMLPath(group, field, entry.Content[i+1])
					deleteYAMLPath(key, field)
				}
			}
			setYAMLPath(group, "keys", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{key}})
		}
		out.Content = append(out.Content, group)
	}
	return out
}

// NormalizeConfigLayout removes conflicting legacy fields. migrate also moves
// legacy-only fields; callers use it only for an explicit v8 configuration write.
func NormalizeConfigLayout(data []byte, migrate bool) ([]byte, bool, error) {
	normalized, changed, err := normalizeConfigLayout(data, migrate)
	return normalized, changed, maskClientKeysIn(data, err)
}

func normalizeConfigLayout(data []byte, migrate bool) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, maskDecoderError(err)
	}
	if len(doc.Content) == 0 {
		if !migrate {
			return data, false, nil
		}
		return nil, false, fmt.Errorf("empty config")
	}
	root := doc.Content[0]
	_, keys, err := flattenV8WithClientKeys(root)
	if err != nil {
		return nil, false, err
	}
	root = expandConfigAliases(root)
	doc.Content[0] = root
	changed, err := normalizeV8PrivateIPAlias(root, migrate)
	if err != nil {
		return nil, false, err
	}
	// Empty legacy structs have no leaf fields to move. Preserve them as empty v8
	// mappings; null structs also mean defaults. User-owned maps are not included.
	paths := append(append([]configPath(nil), v8Aliases...), v8Paths...)
	for _, path := range append(append([]configPath(nil), v8StructPaths...), v8SharedStructPaths...) {
		old := yamlPath(root, path.old)
		if old == nil || (!migrate && yamlPath(root, path.current) == nil) {
			continue
		}
		if old.Tag == "!!null" {
			old.Kind, old.Tag, old.Value = yaml.MappingNode, "!!map", ""
		} else if old.Kind != yaml.MappingNode || len(old.Content) != 0 {
			continue
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		old := legacyPath(root, path.old)
		if old == nil {
			continue
		}
		current := yamlPath(root, path.current)
		if current == nil && !migrate {
			continue
		}
		copy := copyYAMLPathValue(root, path.old)
		deleteYAMLPath(root, path.old)
		if current == nil {
			setYAMLPathWithComments(root, path.current, copy)
		}
		changed = true
	}
	for _, family := range v8KeyFamilies {
		old := yamlPath(root, family.old)
		if old == nil {
			continue
		}
		path := "api-keys." + family.current
		if yamlPath(root, path) == nil {
			if !migrate {
				continue
			}
			setYAMLPath(root, path, groupLegacyKeys(old, family.current))
		}
		deleteYAMLPath(root, family.old)
		changed = true
	}
	if migrate {
		// Existing unknown fields are ignored by the runtime. Retain their
		// contents as comments while keeping new v8 writes strictly validated.
		if err := commentUnknownV8Sections(root, keys); err != nil {
			return nil, false, err
		}
		setYAMLPath(root, "config-version", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "8"})
		changed = true
	}
	if !changed {
		return data, false, nil
	}
	out, err := yaml.Marshal(&doc)
	return out, true, err
}

// IsV8ConfigLayout identifies a validated document by its version or v8 paths.
// Roots shared with the legacy runtime schema do not alone indicate v8.
func IsV8ConfigLayout(root *yaml.Node) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	root = expandConfigAliases(root)
	sections := v8AllowedRoots()
	for _, shared := range []string{"api-keys", "plugins", "quota-exceeded", "routing", "client"} {
		delete(sections, shared)
	}
	for i := 0; i < len(root.Content); i += 2 {
		if sections[root.Content[i].Value] {
			return true
		}
	}
	if keys := yamlPath(root, "api-keys"); keys != nil && keys.Kind == yaml.MappingNode {
		return true
	}
	paths := append(append(append([]configPath(nil), v8Paths...), v8StructPaths...), v8Aliases...)
	for _, path := range paths {
		if path.old != path.current && yamlPath(root, path.current) != nil {
			return true
		}
		if strings.HasPrefix(path.old, "providers.") && yamlPath(root, path.old) != nil {
			return true
		}
		if path.old != path.current && strings.HasPrefix(path.current, "routing.") {
			child, _, _ := strings.Cut(strings.TrimPrefix(path.current, "routing."), ".")
			if yamlPath(root, "routing."+child) != nil {
				return true
			}
		}
	}
	return false
}

func v8AllowedRoots() map[string]bool {
	allowed := map[string]bool{"config-version": true, "api-keys": true, "plugins": true, "quota-exceeded": true, "client": true}
	for _, path := range v8Paths {
		section, _, _ := strings.Cut(path.current, ".")
		allowed[section] = true
	}
	return allowed
}

var (
	v8WarnMu   sync.RWMutex
	v8WarnFunc func(section, msg string)
)

// SetV8MigrationWarnFunc sets a custom warning handler (e.g. from logging package).
func SetV8MigrationWarnFunc(fn func(section, msg string)) {
	v8WarnMu.Lock()
	defer v8WarnMu.Unlock()
	v8WarnFunc = fn
}

func warnUnrecognizedV8Section(section string) {
	msg := fmt.Sprintf("unrecognized configuration section %q commented out during v8 migration", section)
	v8WarnMu.RLock()
	fn := v8WarnFunc
	v8WarnMu.RUnlock()
	if fn != nil {
		fn(section, msg)
		return
	}
	log.Warn(msg)
}

func commentUnknownV8Sections(root *yaml.Node, keys clientKeySet) error {
	if err := commentUnknownV8Fields(root, root, v8AllowedRoots(), "", keys); err != nil {
		return err
	}

	children := make(map[string]map[string]bool)
	for _, path := range append(append([]configPath(nil), v8Paths...), v8StructPaths...) {
		parts := strings.Split(path.current, ".")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], ".")
			if children[parent] == nil {
				children[parent] = make(map[string]bool)
			}
			children[parent][parts[i]] = true
		}
	}
	// Some structs already live at their v8 path and therefore have no remap entry.
	allowedRoots := v8AllowedRoots()
	var includeNative func(reflect.Type, string)
	includeNative = func(t reflect.Type, path string) {
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if tag == "-" || field.PkgPath != "" {
				continue
			}
			if field.Anonymous {
				includeNative(field.Type, path)
				continue
			}
			if path == "" && !allowedRoots[tag] {
				continue
			}
			if children[path] == nil {
				children[path] = make(map[string]bool)
			}
			children[path][tag] = true
			child := tag
			if path != "" {
				child = path + "." + tag
			}
			includeNative(field.Type, child)
		}
	}
	includeNative(reflect.TypeOf(Config{}), "")
	var walk func(*yaml.Node, string) error
	walk = func(node *yaml.Node, path string) error {
		if node == nil || node.Kind != yaml.MappingNode || children[path] == nil {
			return nil
		}
		if err := commentUnknownV8Fields(node, root, children[path], path, keys); err != nil {
			return err
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			child := path + "." + node.Content[i].Value
			if err := walk(node.Content[i+1], child); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if err := walk(root.Content[i+1], root.Content[i].Value); err != nil {
			return err
		}
	}
	return nil
}

// commentUnknownV8Fields archives the fields of node that allowed does not name
// as comments. The archive keeps their text; the warning masks a field named
// like one of the client keys.
func commentUnknownV8Fields(node *yaml.Node, archive *yaml.Node, allowed map[string]bool, path string, keys clientKeySet) error {
	var comments []string
	for i := 0; i+1 < len(node.Content); {
		key := node.Content[i].Value
		if allowed[key] {
			i += 2
			continue
		}
		section, display := key, keys.mask(key)
		if path != "" {
			section, display = path+"."+key, path+"."+display
		}
		warnUnrecognizedV8Section(display)
		keyNode := *node.Content[i]
		keyNode.Value = section
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{&keyNode, node.Content[i+1]}}
		data, errMarshal := yaml.Marshal(entry)
		if errMarshal != nil {
			return errMarshal
		}
		text := strings.TrimSuffix(string(data), "\n")
		comments = append(comments, "# "+strings.ReplaceAll(text, "\n", "\n# "))
		node.Content = append(node.Content[:i], node.Content[i+2:]...)
	}
	// Archive at the root so deleting a neighboring nested field cannot drop it.
	if len(comments) > 0 {
		archive.FootComment = strings.TrimSpace(archive.FootComment + "\n" + strings.Join(comments, "\n"))
	}
	return nil
}

// The deprecated allow flag is the inverse of disable-private-remote-ips.
// Resolve it before applying v8 precedence so both names cannot reach the runtime
// decoder together. Legacy-only files are left untouched until a migration/save.
func normalizeV8PrivateIPAlias(root *yaml.Node, migrate bool) (bool, error) {
	const old = "codex.live-media-relay.allow-private-remote-ips"
	const canonical = "codex.live-media-relay.disable-private-remote-ips"
	value := yamlPath(root, old)
	if value == nil {
		return false, nil
	}
	if yamlPath(root, "oauth.providers."+canonical) != nil {
		return deleteYAMLPath(root, old), nil
	}
	if !migrate || yamlPath(root, canonical) != nil {
		return false, nil
	}
	var allow bool
	if err := value.Decode(&allow); err != nil {
		return false, fmt.Errorf("decode %s: %w", old, err)
	}
	replacement := deepCopyNode(value)
	if err := replacement.Encode(!allow); err != nil {
		return false, err
	}
	setYAMLPath(root, canonical, replacement)
	deleteYAMLPath(root, old)
	return true, nil
}

// restoreV8Layout lets v0 handlers update the effective runtime fields without
// reintroducing their legacy spellings into an existing v8 document.
func restoreV8Layout(root, layout *yaml.Node, original []byte, generated *yaml.Node) error {
	for _, path := range v8Paths {
		upstreams := yamlPath(layout, "api-keys")
		clientKeyCollision := path.old == "api-keys" && upstreams != nil && upstreams.Kind == yaml.MappingNode
		if yamlPath(layout, path.current) == nil && !clientKeyCollision {
			continue
		}
		value := legacyPath(root, path.old)
		if value == nil {
			continue
		}
		copy := copyYAMLPathValue(root, path.old)
		deleteYAMLPath(root, path.old)
		setYAMLPathWithComments(root, path.current, copy)
	}
	var baseline *yaml.Node
	for _, family := range v8KeyFamilies {
		groups := yamlPath(layout, "api-keys."+family.current)
		if groups == nil {
			continue
		}
		if baseline == nil {
			cfg, err := ParseConfigBytes(original)
			if err != nil {
				return err
			}
			baseline = new(yaml.Node)
			if err = baseline.Encode((*legacyConfig)(cfg)); err != nil {
				return err
			}
		}
		var before, after any
		if value := yamlPath(baseline, family.old); value != nil {
			if err := value.Decode(&before); err != nil {
				return err
			}
		}
		if value := yamlPath(generated, family.old); value != nil {
			if err := value.Decode(&after); err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(before, after) {
			keys := yamlPath(root, family.old)
			if keys == nil {
				keys = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			}
			groups = groupLegacyKeys(keys, family.current)
		}
		deleteYAMLPath(root, family.old)
		setYAMLPath(root, "api-keys."+family.current, groups)
	}
	preserveV8Comments(root, layout)
	return nil
}

func preserveV8Comments(dst, src *yaml.Node) {
	if dst == nil || src == nil {
		return
	}
	dst.HeadComment, dst.LineComment, dst.FootComment = src.HeadComment, src.LineComment, src.FootComment
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		if index := findMapKeyIndex(dst, src.Content[i].Value); index >= 0 {
			preserveV8Comments(dst.Content[index], src.Content[i])
			preserveV8Comments(dst.Content[index+1], src.Content[i+1])
		}
	}
}

// Expand aliases and YAML merge keys before moving paths. Keeping an alias to a
// removed legacy anchor would produce an unreadable file, and updating a shared
// anchor in place could unintentionally change an unrelated setting.
// Callers first decode the document so yaml.v3 rejects cyclic/invalid aliases.
func expandConfigAliases(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return expandConfigAliases(node.Alias)
	}
	copy := *node
	copy.Anchor = ""
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = expandConfigAliases(child)
	}
	if copy.Kind != yaml.MappingNode {
		return &copy
	}
	for i := 0; i < len(copy.Content); i += 2 {
		if copy.Content[i].Tag != "!!merge" {
			continue
		}
		merge := copy.Content[i+1]
		copy.Content = append(copy.Content[:i], copy.Content[i+2:]...)
		mappings := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			mappings = merge.Content
		}
		for _, mapping := range mappings {
			for j := 0; j < len(mapping.Content); j += 2 {
				if findMapKeyIndex(&copy, mapping.Content[j].Value) < 0 {
					copy.Content = append(copy.Content, mapping.Content[j], mapping.Content[j+1])
				}
			}
		}
		i -= 2
	}
	return &copy
}

// ValidateV8Config accepts only the v8 layout in management writes.
// Plugin-owned configuration remains extensible through its existing decoder.
func ValidateV8Config(data []byte) error {
	return maskClientKeysIn(data, validateV8Config(data))
}

func validateV8Config(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return maskDecoderError(err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("empty config")
	}
	root := doc.Content[0]
	flat, keys, err := flattenV8WithClientKeys(root)
	if err != nil {
		return err
	}
	root = expandConfigAliases(root)
	allowedRoots := v8AllowedRoots()
	for _, path := range append(append(append([]configPath(nil), v8Paths...), v8Aliases...), v8SharedStructPaths...) {
		if legacyPath(root, path.old) != nil {
			return fmt.Errorf("legacy field %s is not accepted by v8; use %s", path.old, path.current)
		}
	}
	for i := 0; i < len(root.Content); i += 2 {
		if key := root.Content[i].Value; !allowedRoots[key] {
			return fmt.Errorf("unknown v8 configuration section %s", keys.mask(key))
		}
	}
	if groups := yamlPath(root, "api-keys"); groups != nil && groups.Kind == yaml.MappingNode {
		for i := 0; i < len(groups.Content); i += 2 {
			found := false
			for _, family := range v8KeyFamilies {
				if groups.Content[i].Value == family.current {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("unknown API-key provider %s", keys.mask(groups.Content[i].Value))
			}
		}
	}
	deleteYAMLPath(flat, "config-version")
	// Empty struct containers are valid replacements. Only strip known structural
	// paths; empty user maps (headers, aliases, plugin options) carry real values.
	for _, path := range v8Paths {
		parts := strings.Split(path.current, ".")
		for end := len(parts) - 1; end > 0; end-- {
			container := strings.Join(parts[:end], ".")
			if value := yamlPath(flat, container); value != nil && value.Kind == yaml.MappingNode && len(value.Content) == 0 {
				deleteYAMLPath(flat, container)
			}
		}
	}
	encoded, err := yaml.Marshal(flat)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var cfg legacyConfig
	if err = decoder.Decode(&cfg); err != nil {
		return keys.maskUnknownFields(maskDecoderError(err))
	}
	// Client key allowances are the only v8 values with a semantic range check
	// here; ParseConfigBytes runs first in management writes and must not turn
	// them into a 422 before this layout validation reports 400.
	return validateAPIKeyLimits(normalizeAPIKeyLimitKeys(cfg.APIKeyLimits))
}
