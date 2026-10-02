package deploykit_test

import (
	"encoding/json"
	"errors"
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

// breakKinds is every kind of break the corpus names. A kind this test has not heard of fails
// it, so a renamed kind cannot quietly stop a refusal from being checked.
var breakKinds = []string{"missing-required", "wrong-type", "enum", "unknown-field", "union", "rule"}

// decodeRefuses is every kind of break a generated type refuses on decode. An unknown field, a
// union no branch of which matches and a cross-field rule only the schema refuses, so a service
// that must refuse those validates against the schema, not the type.
var decodeRefuses = []string{"missing-required", "wrong-type", "enum"}

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
		{"resolved-deployment", decodeAs[resolved.ResolvedDeployment], decodeRefuses},
		{"composition-lock", decodeAs[lock.CompositionLock], decodeRefuses},
		// The pin annotations are a union of three shapes, which the generator merges into one
		// struct whose every field is optional: within a union, only a field's type holds.
		{"pin-annotations", decodeAs[pins.PinAnnotations], []string{"wrong-type"}},
		{"cluster-state-snapshot", decodeAs[clusterstate.Snapshot], decodeRefuses},
	}
	for _, s := range schemas {
		t.Run(s.name, func(t *testing.T) {
			refusalsChecked := 0
			for _, c := range loadCorpus(t, s.name) {
				t.Run(c.Name, func(t *testing.T) {
					data, err := json.Marshal(c.doc)
					if err != nil {
						t.Fatal(err)
					}
					err = s.decode(data)
					switch {
					case c.Verdict == "accept":
						if err != nil {
							t.Fatalf("an accepted document does not decode: %v", err)
						}
					case c.Verdict == "refuse" && slices.Contains(s.enforced, c.Breaks):
						refusalsChecked++
						if err == nil {
							t.Fatalf("a document breaking %s decodes", c.Breaks)
						}
					}
				})
			}
			if refusalsChecked == 0 {
				t.Fatalf("no case refuses a break of %v, so the decode check never ran", s.enforced)
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
}

type patchStep struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// resolvedCase is a corpus case and the document it names.
type resolvedCase struct {
	corpusCase
	doc any
}

// loadCorpus reads a schema's corpus and resolves every case to its document: an inline instance,
// a spec file, or an earlier case, then the case's JSON Patch applied to it.
func loadCorpus(t *testing.T, name string) []resolvedCase {
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
	byName := map[string]corpusCase{}
	for _, c := range file.Cases {
		if _, dup := byName[c.Name]; dup {
			t.Fatalf("corpus %s names two cases %q", name, c.Name)
		}
		switch {
		case c.Verdict == "accept" && c.Breaks == "":
		case c.Verdict == "refuse" && slices.Contains(breakKinds, c.Breaks):
		default:
			t.Fatalf("case %q: verdict %q breaking %q is not one this test knows", c.Name, c.Verdict, c.Breaks)
		}
		byName[c.Name] = c
	}
	cases := make([]resolvedCase, 0, len(file.Cases))
	for _, c := range file.Cases {
		doc, err := resolveCase(root, byName, c, 0)
		if err != nil {
			t.Fatalf("case %q: %v", c.Name, err)
		}
		cases = append(cases, resolvedCase{c, doc})
	}
	return cases
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
			return nil, fmt.Errorf("%s %s: %w", step.Op, step.Path, err)
		}
	}
	return doc, nil
}

// apply is the add, remove and replace operations of RFC 6902, which is every operation the
// corpus uses.
func apply(doc any, step patchStep) (any, error) {
	if !slices.Contains([]string{"add", "remove", "replace"}, step.Op) {
		return nil, errors.New("unsupported operation")
	}
	if step.Path == "" {
		if step.Op == "remove" {
			return nil, errors.New("cannot remove the whole document")
		}
		return step.Value, nil
	}
	if !strings.HasPrefix(step.Path, "/") {
		return nil, errors.New("a pointer starts with /")
	}
	tokens := strings.Split(step.Path[1:], "/")
	for i, tok := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
	}
	return applyAt(doc, tokens, step)
}

// applyAt applies step at tokens beneath node and returns the node, which an array insertion or
// deletion replaces.
func applyAt(node any, tokens []string, step patchStep) (any, error) {
	tok, last := tokens[0], len(tokens) == 1
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[tok]
		if !ok && (!last || step.Op != "add") {
			return nil, fmt.Errorf("no member %q", tok)
		}
		if !last {
			updated, err := applyAt(child, tokens[1:], step)
			n[tok] = updated
			return n, err
		}
		if step.Op == "remove" {
			delete(n, tok)
		} else {
			n[tok] = step.Value
		}
		return n, nil
	case []any:
		i, err := index(tok, len(n), last && step.Op == "add")
		if err != nil {
			return nil, err
		}
		if !last {
			updated, err := applyAt(n[i], tokens[1:], step)
			n[i] = updated
			return n, err
		}
		switch step.Op {
		case "add":
			return slices.Insert(n, i, step.Value), nil
		case "remove":
			return slices.Delete(n, i, i+1), nil
		default:
			n[i] = step.Value
			return n, nil
		}
	default:
		return nil, fmt.Errorf("cannot descend into %T at %q", node, tok)
	}
}

// index reads an array token. "-" and the length name the end of the array, which only an add
// may address.
func index(tok string, length int, adding bool) (int, error) {
	i := length
	if tok != "-" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return 0, fmt.Errorf("index %q is not a number", tok)
		}
		i = n
	}
	if i < 0 || i > length || (i == length && !adding) {
		return 0, fmt.Errorf("index %q out of range", tok)
	}
	return i, nil
}
