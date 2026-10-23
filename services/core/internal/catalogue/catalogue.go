// Package catalogue loads and validates tool definitions (Spec 03).
package catalogue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/yamlx"
)

// Transition is the state-machine transition a tool performs.
type Transition struct {
	From []string `json:"from"`
	To   string   `json:"to"`
}

// Tool is one tool definition from tools/catalogue/*.yaml.
type Tool struct {
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	Summary         string         `json:"summary"`
	Description     string         `json:"description,omitempty"`
	RiskTier        string         `json:"risk_tier"`
	DefaultAutonomy string         `json:"default_autonomy"`
	MaxAutonomy     string         `json:"max_autonomy,omitempty"`
	Mutates         bool           `json:"mutates"`
	Idempotent      bool           `json:"idempotent"`
	Scopes          []string       `json:"scopes"`
	Consent         []string       `json:"consent,omitempty"`
	Preconditions   []string       `json:"preconditions,omitempty"`
	Transition      *Transition    `json:"transition,omitempty"`
	Emits           []string       `json:"emits,omitempty"`
	Errors          []string       `json:"errors,omitempty"`
	Input           map[string]any `json:"input"`
	Output          map[string]any `json:"output"`

	// Domain is the catalogue file the tool came from.
	Domain string `json:"domain"`

	inputSchema *jsonschema.Schema
}

// Catalogue is the full set of tools, keyed by name.
type Catalogue struct {
	Tools map[string]*Tool
	Names []string // sorted
}

type catalogueFile struct {
	Catalogue   string  `json:"catalogue"`
	Description string  `json:"description"`
	Tools       []*Tool `json:"tools"`
}

// Load reads <dir>/schema/tool-definition.schema.json and every <dir>/catalogue/*.yaml,
// validates each file against the schema and compiles each tool's input schema.
func Load(dir string) (*Catalogue, error) {
	schemaPath := filepath.Join(dir, "schema", "tool-definition.schema.json")
	metaDoc, err := readJSONDoc(schemaPath)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("mem://tamarind/tool-definition.schema.json", metaDoc); err != nil {
		return nil, err
	}
	meta, err := c.Compile("mem://tamarind/tool-definition.schema.json")
	if err != nil {
		return nil, fmt.Errorf("compile tool meta-schema: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "catalogue", "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no catalogue files in %s", filepath.Join(dir, "catalogue"))
	}

	cat := &Catalogue{Tools: map[string]*Tool{}}
	inputCompiler := jsonschema.NewCompiler()
	inputCompiler.DefaultDraft(jsonschema.Draft2020)
	inputCompiler.AssertFormat()

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		js, err := yamlx.ToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			return nil, err
		}
		if err := meta.Validate(doc); err != nil {
			return nil, fmt.Errorf("%s does not match the tool definition schema: %v", filepath.Base(f), err)
		}
		var cf catalogueFile
		if err := json.Unmarshal(js, &cf); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, t := range cf.Tools {
			if _, dup := cat.Tools[t.Name]; dup {
				return nil, fmt.Errorf("duplicate tool %q in %s", t.Name, filepath.Base(f))
			}
			t.Domain = cf.Catalogue
			url := "mem://tamarind/tools/" + t.Name + "/input.json"
			inJSON, _ := json.Marshal(t.Input)
			inDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(inJSON))
			if err != nil {
				return nil, err
			}
			if err := inputCompiler.AddResource(url, inDoc); err != nil {
				return nil, fmt.Errorf("tool %s: %w", t.Name, err)
			}
			if t.inputSchema, err = inputCompiler.Compile(url); err != nil {
				return nil, fmt.Errorf("tool %s: invalid input schema: %w", t.Name, err)
			}
			cat.Tools[t.Name] = t
			cat.Names = append(cat.Names, t.Name)
		}
	}
	sort.Strings(cat.Names)
	return cat, nil
}

// ValidateInput checks a tool input against the tool's JSON Schema.
func (t *Tool) ValidateInput(input map[string]any) error {
	js, err := json.Marshal(input)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return err
	}
	if err := t.inputSchema.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			return fmt.Errorf("%s", flatten(ve))
		}
		return err
	}
	return nil
}

func asValidationError(err error, out **jsonschema.ValidationError) bool {
	ve, ok := err.(*jsonschema.ValidationError)
	if ok {
		*out = ve
	}
	return ok
}

var printer = message.NewPrinter(language.English)

// flatten turns a nested validation error into "path: message; path: message".
func flatten(ve *jsonschema.ValidationError) string {
	var parts []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			parts = append(parts, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return strings.Join(parts, "; ")
}

func readJSONDoc(path string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jsonschema.UnmarshalJSON(f)
}
