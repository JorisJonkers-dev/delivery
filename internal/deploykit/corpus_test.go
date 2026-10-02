package deploykit_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/clusterstate"
	"github.com/JorisJonkers-dev/delivery/internal/deploykit/lock"
	"github.com/JorisJonkers-dev/delivery/internal/deploykit/pins"
	"github.com/JorisJonkers-dev/delivery/internal/deploykit/resolved"
)

// vendored mirrors deploy-kit's spec/v1, so a path a corpus case names resolves beneath it.
const vendored = "../../third_party/deploy-kit"

// required is every kind of break a generated type refuses on decode. An unknown field, a union
// no branch of which matches and a cross-field rule only the schema refuses, so a service that
// must refuse those validates against the schema, not the type.
var required = []string{"missing-required", "wrong-type", "enum"}

func decodeAs[T any](data []byte) error {
	var v T
	return json.Unmarshal(data, &v)
}

func TestTypesAgreeWithTheCorpus(t *testing.T) {
	schemas := []struct {
		name     string
		decode   func([]byte) error
		enforced []string
	}{
		{"resolved-deployment", decodeAs[resolved.ResolvedDeployment], required},
		{"composition-lock", decodeAs[lock.CompositionLock], required},
		// The pin annotations are a union of three shapes, which the generator merges into one
		// struct whose every field is optional: within a union, only a field's type holds.
		{"pin-annotations", decodeAs[pins.PinAnnotations], []string{"wrong-type"}},
		{"cluster-state-snapshot", decodeAs[clusterstate.Snapshot], required},
	}
	for _, s := range schemas {
		t.Run(s.name, func(t *testing.T) {
			cases := loadCorpus(t, s.name)
			for _, c := range cases {
				t.Run(c.Name, func(t *testing.T) {
					data, err := json.Marshal(c.resolved)
					if err != nil {
						t.Fatal(err)
					}
					err = s.decode(data)
					switch {
					case c.Verdict == "accept" && err != nil:
						t.Fatalf("an accepted document does not decode: %v", err)
					case c.Verdict == "refuse" && slices.Contains(s.enforced, c.Breaks) && err == nil:
						t.Fatalf("a document breaking %s decodes", c.Breaks)
					}
				})
			}
		})
	}
}

type corpusCase struct {
	Name     string      `json:"name"`
	Verdict  string      `json:"verdict"`
	Breaks   string      `json:"breaks"`
	Case     string      `json:"case"`
	File     string      `json:"file"`
	Instance any         `json:"instance"`
	Patch    []patchStep `json:"patch"`
	resolved any
}

type patchStep struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// loadCorpus reads a schema's corpus and resolves every case to its document: an inline instance,
// a spec file, or an earlier case, then the case's JSON Patch applied to it.
func loadCorpus(t *testing.T, name string) []corpusCase {
	t.Helper()
	root, err := os.OpenRoot(vendored)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	raw, err := root.ReadFile(path.Join("schemas", "corpus", name+".corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Schema string       `json:"schema"`
		Cases  []corpusCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Schema != name+".schema.json" {
		t.Fatalf("corpus %s names schema %q", name, file.Schema)
	}
	if len(file.Cases) == 0 {
		t.Fatalf("corpus %s has no cases", name)
	}
	byName := map[string]corpusCase{}
	for _, c := range file.Cases {
		byName[c.Name] = c
	}
	for i := range file.Cases {
		doc, err := resolveCase(root, byName, file.Cases[i], 0)
		if err != nil {
			t.Fatalf("case %q: %v", file.Cases[i].Name, err)
		}
		file.Cases[i].resolved = doc
	}
	return file.Cases
}

func resolveCase(root *os.Root, byName map[string]corpusCase, c corpusCase, depth int) (any, error) {
	var doc any
	switch {
	case c.File != "":
		raw, err := root.ReadFile(c.File)
		if err != nil {
			return nil, err
		}
		// JSON is YAML, so one reader takes both kinds of file a case can name.
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	case c.Case != "":
		base, ok := byName[c.Case]
		if !ok {
			return nil, fmt.Errorf("no base case %q", c.Case)
		}
		if depth > len(byName) {
			return nil, fmt.Errorf("case %q refers back to itself", c.Name)
		}
		var err error
		if doc, err = resolveCase(root, byName, base, depth+1); err != nil {
			return nil, err
		}
	default:
		doc = c.Instance
	}
	// A deep copy, so patching one case never changes the document another case reads.
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, step := range c.Patch {
		if doc, err = apply(doc, step); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// apply is the add, remove and replace operations of RFC 6902, which is every operation the
// corpus uses.
func apply(doc any, step patchStep) (any, error) {
	tokens, err := pointer(step.Path)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		if step.Op == "remove" {
			return nil, fmt.Errorf("cannot remove the whole document")
		}
		return step.Value, nil
	}
	parent, err := walk(doc, tokens[:len(tokens)-1])
	if err != nil {
		return nil, err
	}
	last := tokens[len(tokens)-1]
	switch p := parent.(type) {
	case map[string]any:
		if _, ok := p[last]; !ok && step.Op != "add" {
			return nil, fmt.Errorf("%s %s: no such member", step.Op, step.Path)
		}
		if step.Op == "remove" {
			delete(p, last)
		} else {
			p[last] = step.Value
		}
		return doc, nil
	case []any:
		return doc, applyToArray(doc, tokens[:len(tokens)-1], p, last, step)
	default:
		return nil, fmt.Errorf("%s %s: parent is neither object nor array", step.Op, step.Path)
	}
}

func applyToArray(doc any, parentPath []string, arr []any, last string, step patchStep) error {
	i := len(arr)
	if last != "-" {
		n, err := strconv.Atoi(last)
		if err != nil || n < 0 || n > len(arr) || (n == len(arr) && step.Op != "add") {
			return fmt.Errorf("%s %s: index out of range", step.Op, step.Path)
		}
		i = n
	}
	switch step.Op {
	case "add":
		arr = slices.Insert(arr, i, step.Value)
	case "remove":
		arr = slices.Delete(arr, i, i+1)
	case "replace":
		arr[i] = step.Value
		return nil
	default:
		return fmt.Errorf("unsupported op %q", step.Op)
	}
	return set(doc, parentPath, arr)
}

// set replaces the value at tokens, which is how a resized array reaches its parent.
func set(doc any, tokens []string, value any) error {
	if len(tokens) == 0 {
		return fmt.Errorf("cannot resize the root array")
	}
	parent, err := walk(doc, tokens[:len(tokens)-1])
	if err != nil {
		return err
	}
	last := tokens[len(tokens)-1]
	switch p := parent.(type) {
	case map[string]any:
		p[last] = value
	case []any:
		n, err := strconv.Atoi(last)
		if err != nil || n < 0 || n >= len(p) {
			return fmt.Errorf("index %q out of range", last)
		}
		p[n] = value
	default:
		return fmt.Errorf("parent is neither object nor array")
	}
	return nil
}

func walk(doc any, tokens []string) (any, error) {
	for _, tok := range tokens {
		switch d := doc.(type) {
		case map[string]any:
			next, ok := d[tok]
			if !ok {
				return nil, fmt.Errorf("no member %q", tok)
			}
			doc = next
		case []any:
			n, err := strconv.Atoi(tok)
			if err != nil || n < 0 || n >= len(d) {
				return nil, fmt.Errorf("index %q out of range", tok)
			}
			doc = d[n]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", doc, tok)
		}
	}
	return doc, nil
}

func pointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("pointer %q does not start with /", path)
	}
	tokens := strings.Split(path[1:], "/")
	for i, tok := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}
