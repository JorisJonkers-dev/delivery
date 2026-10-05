// Package declared_test holds the two statements of each service's cluster grant to each other:
// the `api` block deploy/delivery.project.yml declares, which deploy-kit renders into a
// ClusterRole, and the ClusterRole deploy/<service>/rbac.yaml holds as an object, which the
// envtest cases apply. A rule in one and not the other is a grant the tests do not prove, or a
// test that proves a grant nothing renders.
package declared_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

type project struct {
	Applications []struct {
		ID        string `json:"id"`
		Processes []struct {
			Name string `json:"name"`
			API  *struct {
				Reason string `json:"reason"`
				Rules  []struct {
					Group   string   `json:"group"`
					Objects []string `json:"objects"`
					Verbs   []string `json:"verbs"`
				} `json:"rules"`
			} `json:"api"`
		} `json:"processes"`
	} `json:"applications"`
}

// grant is one thing a service may ask of the API: a verb on an object of a group.
func grants(group string, objects, verbs []string) []string {
	var out []string
	for _, object := range objects {
		for _, verb := range verbs {
			out = append(out, group+"/"+object+":"+verb)
		}
	}
	return out
}

// declared is every grant a Process of the project file declares, by Process, with the core
// group spelled as Kubernetes spells it.
func declared(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/delivery.project.yml")
	if err != nil {
		t.Fatal(err)
	}
	var p project
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, application := range p.Applications {
		for _, process := range application.Processes {
			if process.API == nil {
				continue
			}
			if strings.TrimSpace(process.API.Reason) == "" {
				t.Errorf("%s declares API access and no reason", process.Name)
			}
			for _, rule := range process.API.Rules {
				group := rule.Group
				if group == "core" {
					group = ""
				}
				out[process.Name] = append(out[process.Name], grants(group, rule.Objects, rule.Verbs)...)
			}
			slices.Sort(out[process.Name])
		}
	}
	return out
}

// held is every grant the ClusterRole in a hand-kept rbac.yaml holds.
func held(t *testing.T, service string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/" + service + "/rbac.yaml") //nolint:gosec // G304: one of the two services this test names
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for document := range strings.SplitSeq(string(raw), "\n---\n") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatal(err)
		}
		if role.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				out = append(out, grants(group, rule.Resources, rule.Verbs)...)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestEachServicesRbacFileHoldsExactlyWhatItsProcessDeclares(t *testing.T) {
	all := declared(t)
	for _, service := range []string{"release-gate", "collector"} {
		t.Run(service, func(t *testing.T) {
			if got, want := held(t, service), all[service]; !slices.Equal(got, want) {
				t.Fatalf("deploy/%s/rbac.yaml holds\n%v\nand the project file declares\n%v", service, got, want)
			}
		})
	}
}

func TestEveryHolderOfApiAccessIsOneThePlatformIsAskedToAdmit(t *testing.T) {
	// The three the README names; a fourth would need the Platform document's list to grow.
	var holders []string
	for name := range declared(t) {
		holders = append(holders, name)
	}
	slices.Sort(holders)
	if want := []string{"collector", "flagger", "release-gate"}; !slices.Equal(holders, want) {
		t.Fatalf("holders = %v, want %v", holders, want)
	}
}
