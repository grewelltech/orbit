package scenario

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ueName is what a ues[] entry's name may be: letters, digits and '-', and
// not all digits, so a name is never mistaken for a SUPI.
var ueName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// resolveSteps checks and rewrites the steps' UE references and echo
// destinations, once the scenario's SUPIs are known:
//   - a step's ue: (or register:/deregister:'s value) is "all", a UE's
//     name, or a literal SUPI; a name becomes that UE's SUPI. With a target
//     in use the SUPIs are allocated, so a literal SUPI is an error.
//   - with a target in use, an omitted ping.dst or latency.target takes
//     the target's first responder's data_ip; with no responder that is an
//     error naming the step and the field.
func (s *Scenario) resolveSteps() error {
	names := map[string]string{}
	for i, u := range s.UEs {
		if u.Name == "" {
			continue
		}
		switch {
		case !ueName.MatchString(u.Name) || strings.Trim(u.Name, "0123456789") == "":
			return fmt.Errorf("ues[%d].name %q: use letters, digits and '-', not digits only", i, u.Name)
		case u.Range != nil:
			return fmt.Errorf("ues[%d].name %q: a range entry has no name; steps reach its UEs through all", i, u.Name)
		case names[u.Name] != "":
			return fmt.Errorf("ues[%d].name %q is used twice", i, u.Name)
		}
		names[u.Name] = u.SUPI
	}
	targeted := s.targetName != ""
	ref := func(step int, v string) (string, error) {
		switch {
		case v == "" || v == "all":
			return v, nil
		case names[v] != "":
			return names[v], nil
		case strings.Trim(strings.TrimPrefix(v, "imsi-"), "0123456789") == "":
			if targeted {
				return "", fmt.Errorf("step %d refers to UE %s; with a target the SUPIs are allocated, so name the UE (ues[].name) and refer to it", step, v)
			}
			return v, nil
		}
		return "", fmt.Errorf("step %d refers to UE %q, which no ues[] entry names", step, v)
	}
	for i := range s.Steps {
		st := &s.Steps[i]
		n := i + 1
		switch st.value.Kind {
		case yaml.ScalarNode:
			if st.Action == "register" || st.Action == "deregister" {
				v, err := ref(n, st.value.Value)
				if err != nil {
					return err
				}
				st.value.Value = v
			}
		case yaml.MappingNode:
			if u := mapKey(&st.value, "ue"); u != nil {
				v, err := ref(n, u.Value)
				if err != nil {
					return err
				}
				u.Value = v
			}
			field := map[string]string{"ping": "dst", "latency": "target"}[st.Action]
			if field == "" || !targeted || mapKey(&st.value, field) != nil {
				continue
			}
			if s.echo == "" {
				return fmt.Errorf("step %d %s.%s not set and target %s has no responder (with a data_ip); set %s.%s or add a responder",
					n, st.Action, field, s.targetName, st.Action, field)
			}
			st.value.Content = append(st.value.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: field},
				&yaml.Node{Kind: yaml.ScalarNode, Value: s.echo})
		}
	}
	return nil
}
