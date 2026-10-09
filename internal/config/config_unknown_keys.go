package config

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// extraConfigKeyPaths are consumed without a struct field or a v8 path entry.
var extraConfigKeyPaths = []string{"config-version", "codex.live-media-relay.allow-private-remote-ips"}

// checkedCustomDecoders are the struct types with their own UnmarshalYAML that
// decode exactly their yaml fields (extra keys are in extraConfigKeyPaths).
// Other struct types with a custom decoder may accept any key and are not checked.
var checkedCustomDecoders = map[reflect.Type]bool{
	reflect.TypeOf(CredentialConcurrencyConfig{}): true,
	reflect.TypeOf(CodexLiveMediaRelayConfig{}):   true,
}

var knownConfigKeys = sync.OnceValue(buildKnownConfigKeys)

// buildKnownConfigKeys maps every mapping path the runtime decodes as a struct,
// in either layout, to the keys it consumes there. Paths it does not name, such
// as user-owned maps and list items, are never checked.
func buildKnownConfigKeys() map[string]map[string]bool {
	known := make(map[string]map[string]bool)
	open := make(map[string]bool)
	add := func(path string) {
		parent := ""
		for _, part := range strings.Split(path, ".") {
			if known[parent] == nil {
				known[parent] = make(map[string]bool)
			}
			known[parent][part] = true
			parent = strings.TrimPrefix(parent+"."+part, ".")
		}
	}
	unmarshaler := reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()
	// The native (legacy) layout, keyed the way yaml.v3 keys struct fields.
	var native func(reflect.Type, string)
	native = func(t reflect.Type, path string) {
		if known[path] == nil {
			known[path] = make(map[string]bool)
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, flags, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if name == "-" || (field.PkgPath != "" && !field.Anonymous) {
				continue
			}
			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if strings.Contains(","+flags+",", ",inline,") {
				if fieldType.Kind() == reflect.Struct {
					native(fieldType, path)
				} else {
					open[path] = true
				}
				continue
			}
			if name == "" {
				name = strings.ToLower(field.Name)
			}
			child := strings.TrimPrefix(path+"."+name, ".")
			add(child)
			if fieldType.Kind() == reflect.Struct && (!reflect.PointerTo(fieldType).Implements(unmarshaler) || checkedCustomDecoders[fieldType]) {
				native(fieldType, child)
			}
		}
	}
	native(reflect.TypeOf(Config{}), "")
	// The v8 layout and the historical aliases. Legacy struct paths are covered
	// by the native walk above.
	for _, path := range append(append([]configPath(nil), v8Paths...), v8StructPaths...) {
		add(path.current)
	}
	for _, path := range append(append([]configPath(nil), v8Aliases...), v8SharedStructPaths...) {
		add(path.old)
		add(path.current)
	}
	// The v8 api-keys map holds one upstream key list per provider family.
	for _, family := range v8KeyFamilies {
		add("api-keys." + family.current)
	}
	for _, path := range extraConfigKeyPaths {
		add(path)
	}
	for path := range open {
		delete(known, path)
	}
	return known
}

// unknownConfigKeys returns a warning for every mapping key in the config
// document data that the runtime decoder ignores. Client keys used as mapping
// keys are masked and values are never printed.
func unknownConfigKeys(data []byte) []string {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	clientKeys, _ := clientKeysOf(doc.Content[0])
	known := knownConfigKeys()
	var findings []string
	var walk func(node *yaml.Node, path, display string)
	walk = func(node *yaml.Node, path, display string) {
		allowed := known[path]
		if node.Kind != yaml.MappingNode || allowed == nil {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode {
				continue
			}
			name := clientKeys.mask(key.Value)
			if path == "api-keys" && !allowed[key.Value] {
				// Client keys written here as a mapping instead of a list are
				// not in clientKeys, so every unknown entry is masked.
				name = maskClientKey(key.Value)
			}
			child := strings.TrimPrefix(path+"."+key.Value, ".")
			childDisplay := strings.TrimPrefix(display+"."+name, ".")
			if !allowed[key.Value] {
				findings = append(findings, fmt.Sprintf("config: unknown key %q (line %d) is ignored", clientKeys.maskText(childDisplay), key.Line))
				continue
			}
			walk(node.Content[i+1], child, childDisplay)
		}
	}
	// Aliases and merge keys are resolved the way the runtime decoder resolves them.
	walk(expandConfigAliases(doc.Content[0]), "", "")
	return findings
}

// warnUnknownConfigKeys logs the keys of the config document data that the
// runtime ignores. It is diagnostic only and never changes what loads.
func warnUnknownConfigKeys(data []byte) {
	for _, finding := range unknownConfigKeys(data) {
		log.Warn(finding)
	}
}
