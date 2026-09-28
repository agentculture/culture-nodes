package decl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"sigs.k8s.io/yaml"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
)

const schemaName = "declaration/declaration.schema.json"

var validator struct {
	sync.Once
	value *contracts.Validator
	err   error
}

// Parse checks the single-unit shape and normalizes authoring defaults.
func Parse(source []byte, format Format) (*Declaration, error) {
	var data []byte
	switch format {
	case FormatJSON:
		data = source
	case FormatYAML:
		var err error
		data, err = yaml.YAMLToJSONStrict(source)
		if err != nil {
			if strings.Contains(err.Error(), "trigger") || strings.Contains(err.Error(), "action") {
				return nil, fmt.Errorf("declaration YAML: %w; use fan-out as separate declarations from the same start node", err)
			}
			return nil, fmt.Errorf("declaration YAML: %w", err)
		}
	default:
		return nil, fmt.Errorf("declaration: unsupported format %q", format)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("declaration JSON: %w", err)
	}
	if shape == nil {
		return nil, fmt.Errorf("declaration must be an object")
	}
	if _, ok := shape["triggers"]; ok {
		return nil, fmt.Errorf("declaration has multiple triggers: use fan-out as separate declarations from the same start node")
	}
	if _, ok := shape["actions"]; ok {
		return nil, fmt.Errorf("declaration has multiple actions: use fan-out as separate declarations from the same start node")
	}
	validator.Do(func() { validator.value, validator.err = contracts.NewValidator() })
	if validator.err != nil {
		return nil, validator.err
	}
	if err := validator.value.ValidateJSON(schemaName, data); err != nil {
		return nil, err
	}
	var d Declaration
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("declaration decode: %w", err)
	}
	if _, ok := kinds.Trigger(d.Trigger.Kind); !ok {
		return nil, fmt.Errorf("declaration trigger kind %q is not in the registered vocabulary (internal/decl/kinds)", d.Trigger.Kind)
	}
	if _, ok := kinds.Action(d.Action.Kind); !ok {
		return nil, fmt.Errorf("declaration action kind %q is not in the registered vocabulary (internal/decl/kinds)", d.Action.Kind)
	}
	if d.Action.Kind == "agent.work" && len(d.Action.With) > 0 {
		var with struct {
			GraphConfig struct {
				Contract struct {
					Outcomes map[string]json.RawMessage `json:"outcomes"`
				} `json:"contract"`
			} `json:"graph_config"`
		}
		if err := json.Unmarshal(d.Action.With, &with); err != nil {
			return nil, fmt.Errorf("declaration agent contract: %w", err)
		}
		if _, redefined := with.GraphConfig.Contract.Outcomes["blocked"]; redefined {
			return nil, fmt.Errorf("declaration agent contract: blocked is a reserved conventional outcome")
		}
	}
	if _, ok := shape["condition"]; !ok {
		d.Condition = "true"
	}
	for _, entry := range d.Exposes {
		if _, err := ParseExposureEntry(entry); err != nil {
			return nil, err
		}
	}
	d.Exposes = NormalizeExposes(d.Exposes)
	if d.StartFrom != nil {
		if err := d.StartFrom.Validate(); err != nil {
			return nil, fmt.Errorf("declaration %w", err)
		}
	}
	var trigger map[string]json.RawMessage
	if err := json.Unmarshal(shape["trigger"], &trigger); err != nil {
		return nil, err
	}
	if _, ok := trigger["reentry_limit"]; !ok {
		d.Trigger.ReentryLimit = 3
	}
	if _, ok := trigger["hop_limit"]; !ok {
		d.Trigger.HopLimit = 20
	}
	if _, ok := trigger["rate_ceiling"]; !ok {
		d.Trigger.RateCeiling = "30/h"
	}
	return &d, nil
}

// rejectDuplicateKeys preserves the one-trigger/one-action rule for JSON,
// where encoding/json otherwise silently accepts the last spelling of a key.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("declaration JSON has trailing content")
		}
		return err
	}
	return nil
}

func scanValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("declaration JSON: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("declaration JSON object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("declaration JSON repeats %q; use fan-out as separate declarations for multiple triggers or actions", key)
			}
			seen[key] = true
			if err := scanValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("declaration JSON has unexpected delimiter %q", delim)
	}
}
