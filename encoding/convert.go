package encoding

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ToYaml converts input JSON (or YAML) data into YAML while preserving key order.
// It unmarshalls data into yaml.Node instead of map[string]interface{} because
// maps do not preserve field order.
func ToYaml(data []byte) ([]byte, error) {
	var out yaml.Node
	err := Unmarshal(data, &out)
	if err != nil {
		return nil, err
	}

	if len(out.Content) == 0 {
		return nil, fmt.Errorf("No content found in the yaml file.")
	}

	// Recursively set the style of nodes to block style for readable formatting.
	setBlockStyle(out.Content[0])

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err = enc.Encode(out.Content[0])
	return buf.Bytes(), err
}

// setBlockStyle recursively resets the style of the node in yaml.Node and its children to block style.
// In simple terms, it makes the output print on multiple indented lines
// instead of a single inline line (which looks like JSON).
func setBlockStyle(n *yaml.Node) {
	n.Style = 0
	for _, child := range n.Content {
		setBlockStyle(child)
	}
}
