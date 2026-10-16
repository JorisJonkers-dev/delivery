package vaultpolicy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Key is one generated key the render names: a key of a KV document under the Project's own
// namespace, written once when absent and never changed
// (deploy-kit spec/v1/30-deliverables.md#vault-configuration-is-rendered-not-applied).
type Key struct {
	Path     string    `json:"path"`
	Key      string    `json:"key"`
	Bytes    int       `json:"bytes"`
	Encoding string    `json:"encoding"`
	Adopt    *Location `json:"adopt,omitempty"`
}

// Location is a key of a KV document whose value a generated key starts from, while Vault does
// not hold the generated key.
type Location struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

const (
	minBytes = 16
	maxBytes = 512
)

var keyName = regexp.MustCompile(`^` + segment + `$`) //nolint:gochecknoglobals // a compiled constant

var kvPath = regexp.MustCompile(`^secret/data(/` + segment + `)+$`) //nolint:gochecknoglobals // a compiled constant

var encodings = map[string]func([]byte) string{ //nolint:gochecknoglobals // a constant table
	"hex":       hex.EncodeToString,
	"base64":    base64.StdEncoding.EncodeToString,
	"base64url": base64.RawURLEncoding.EncodeToString,
}

// ReadKeys reads every `<namespace>.generated.json` the render wrote and holds each key to it:
// a path below `secret/data/<namespace>/`, a key name, a length and an encoding the render
// offers, and no key named twice. Anything else is refused whole.
func ReadKeys(dir string) ([]Key, error) {
	_, _, generated, err := load(dir)
	if err != nil {
		return nil, err
	}
	var keys []Key
	seen := map[string]bool{}
	for _, namespace := range sortedKeys(generated) {
		project, isNamespace := strings.CutSuffix(namespace, namespaceSuffix)
		if !isNamespace || !part.MatchString(project) {
			return nil, fmt.Errorf("vaultpolicy: %s%s names no namespace: %w", namespace, keysSuffix, errNotRendered)
		}
		var document struct {
			Keys []Key `json:"keys"`
		}
		if err := decode(generated[namespace], &document); err != nil {
			return nil, fmt.Errorf("vaultpolicy: %s%s does not parse: %w", namespace, keysSuffix, err)
		}
		below := regexp.MustCompile(`^secret/data/` + regexp.QuoteMeta(namespace) + `(/` + segment + `)+$`)
		for _, k := range document.Keys {
			_, encodes := encodings[k.Encoding]
			switch {
			case !below.MatchString(k.Path):
				return nil, fmt.Errorf("vaultpolicy: %s generates under %q, outside %s: %w", namespace, k.Path, namespace, errNotRendered)
			case !keyName.MatchString(k.Key):
				return nil, fmt.Errorf("vaultpolicy: %s generates the key %q: %w", namespace, k.Key, errNotRendered)
			case k.Bytes < minBytes || k.Bytes > maxBytes:
				return nil, fmt.Errorf("vaultpolicy: %s generates %d bytes for %s: %w", namespace, k.Bytes, k.Key, errNotRendered)
			case !encodes:
				return nil, fmt.Errorf("vaultpolicy: %s encodes %s as %q: %w", namespace, k.Key, k.Encoding, errNotRendered)
			case k.Adopt != nil && (!kvPath.MatchString(k.Adopt.Path) || !keyName.MatchString(k.Adopt.Key)):
				return nil, fmt.Errorf("vaultpolicy: %s adopts %s from %q#%q: %w", namespace, k.Key, k.Adopt.Path, k.Adopt.Key, errNotRendered)
			case seen[k.Path+"#"+k.Key]:
				return nil, fmt.Errorf("vaultpolicy: %s#%s is generated twice: %w", k.Path, k.Key, errNotRendered)
			}
			seen[k.Path+"#"+k.Key] = true
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// ApplyKeys writes each generated key Vault does not hold, path by path, and says which it
// wrote. A key Vault holds, of any value, is left alone. The write is check-and-set against the
// version read, so a path someone wrote in between is never overwritten: the run fails, and the
// next run reads it again.
func ApplyKeys(ctx context.Context, vault Vault, keys []Key) ([]string, error) {
	byPath := map[string][]Key{}
	for _, k := range keys {
		byPath[k.Path] = append(byPath[k.Path], k)
	}
	var written []string
	for _, path := range sortedKeys(byPath) {
		added, err := fill(ctx, vault, path, byPath[path])
		written = append(written, added...)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// fill writes the keys of one path that Vault does not hold, and says which.
func fill(ctx context.Context, vault Vault, path string, keys []Key) ([]string, error) {
	held, version, err := vault.kv(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("vaultpolicy: read %s: %w", path, err)
	}
	data := map[string]json.RawMessage{}
	for name, value := range held {
		data[name] = value
	}
	var added []string
	for _, k := range keys {
		if _, present := held[k.Key]; present {
			continue
		}
		value, from, err := adopted(ctx, vault, k)
		if err != nil {
			return nil, err
		}
		if value == nil {
			if value, err = generate(k); err != nil {
				return nil, err
			}
		}
		data[k.Key] = value
		added = append(added, "key "+path+"#"+k.Key+from)
	}
	if len(added) == 0 {
		return nil, nil
	}
	if err := vault.putKV(ctx, path, data, version); err != nil {
		return nil, fmt.Errorf("vaultpolicy: write %s: %w", path, err)
	}
	return added, nil
}

// adopted is the value k's old location holds, copied unchanged, and how the report names it;
// nothing where k adopts none or the location holds none.
func adopted(ctx context.Context, vault Vault, k Key) (json.RawMessage, string, error) {
	if k.Adopt == nil {
		return nil, "", nil
	}
	old, _, err := vault.kv(ctx, k.Adopt.Path)
	if err != nil {
		return nil, "", fmt.Errorf("vaultpolicy: read %s: %w", k.Adopt.Path, err)
	}
	value, present := old[k.Adopt.Key]
	if !present || string(value) == "null" {
		return nil, "", nil
	}
	return value, " (adopted from " + k.Adopt.Path + "#" + k.Adopt.Key + ")", nil
}

var errShortRead = errors.New("short random read")

// generate is a fresh random value for k, encoded and ready to store.
func generate(k Key) (json.RawMessage, error) {
	raw := make([]byte, k.Bytes)
	if n, err := rand.Read(raw); err != nil || n != k.Bytes {
		return nil, fmt.Errorf("vaultpolicy: generate %s: %w", k.Key, errors.Join(err, errShortRead))
	}
	return json.Marshal(encodings[k.Encoding](raw))
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
