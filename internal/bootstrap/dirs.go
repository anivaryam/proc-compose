package bootstrap

import (
	"bytes"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// verificationConfig returns the config contents to verify.
//
// The verification copy lives in the system temp dir, so a relative dir value
// would resolve against the temp dir instead of the project. The dir values
// are rewritten to absolute paths so the copy resolves like the config that
// will actually run.
//
// A rewrite failure is not fatal: the unmodified suggestion is verified
// instead.
func verificationConfig(suggestedYAML, configPath string) string {
	rewritten, err := absProcessDirs([]byte(suggestedYAML), filepath.Dir(configPath))
	if err != nil {
		return suggestedYAML
	}
	return string(rewritten)
}

// absProcessDirs rewrites the `dir:` values of a proc-compose config so they
// resolve against baseDir instead of the directory the config file happens to
// live in.
//
// A process without a `dir:` key resolves to the config file's directory, so
// one is materialised for those processes too. Every other field is left
// untouched.
func absProcessDirs(contents []byte, baseDir string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(contents, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse config: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("config is empty")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	processes := mappingValue(root, "processes")
	if processes == nil || processes.Kind != yaml.MappingNode {
		// Nothing to rewrite.
		return contents, nil
	}
	for i := 0; i+1 < len(processes.Content); i += 2 {
		proc := processes.Content[i+1]
		if proc.Kind != yaml.MappingNode {
			continue
		}
		setProcessDir(proc, baseDir)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("cannot encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("cannot encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// mappingValue returns the value node for key in a mapping node, or nil when
// the key is absent.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setProcessDir points a process's `dir:` at baseDir. A relative dir is
// joined onto baseDir, an absolute dir is kept, and a process without a
// `dir:` key gets one because such a process resolves to the config file's
// directory.
func setProcessDir(proc *yaml.Node, baseDir string) {
	for i := 0; i+1 < len(proc.Content); i += 2 {
		key := proc.Content[i]
		value := proc.Content[i+1]
		if key.Value != "dir" {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return
		}
		value.Tag = "!!str"
		value.Style = 0
		value.Value = absoluteDir(value.Value, baseDir)
		return
	}
	proc.Content = append(proc.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "dir"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: baseDir},
	)
}

// absoluteDir resolves dir against baseDir unless it is already absolute.
func absoluteDir(dir, baseDir string) string {
	if dir == "" {
		return baseDir
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(baseDir, dir)
}
