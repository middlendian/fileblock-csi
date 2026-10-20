package deploy

import (
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

const policyFile = "kustomize/base/remediation-policy.yaml"

// foldedExpressions returns every `expression: >-` folded block in the
// file, in order, joined with single spaces as YAML folding does. A
// minimal parser is enough: the policy uses only this one form.
func foldedExpressions(t *testing.T, doc string) []string {
	t.Helper()
	var out []string
	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasSuffix(trimmed, "expression: >-") {
			continue
		}
		indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " -"))
		var parts []string
		for i+1 < len(lines) {
			next := lines[i+1]
			if strings.TrimSpace(next) == "" || len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			parts = append(parts, strings.TrimSpace(next))
			i++
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

func celEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		cel.OptionalTypes(),
		ext.Strings(),
		cel.Variable("request", cel.DynType),
		cel.Variable("oldObject", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func evalBool(t *testing.T, env *cel.Env, expr string, vars map[string]any) bool {
	t.Helper()
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("compile %q: %v", expr, iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := prg.Eval(vars)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	b, ok := v.Value().(bool)
	if !ok {
		t.Fatalf("%q returned %T, want bool", expr, v.Value())
	}
	return b
}

const nodeSA = "system:serviceaccount:fileblock-system:fileblock-node"

func request(user, node string) map[string]any {
	extra := map[string]any{}
	if node != "" {
		extra["authentication.kubernetes.io/node-name"] = []any{node}
	}
	return map[string]any{"userInfo": map[string]any{"username": user, "extra": extra}}
}

func pod(node string, owners []any, volumes []any) map[string]any {
	meta := map[string]any{}
	if owners != nil {
		meta["ownerReferences"] = owners
	}
	return map[string]any{
		"metadata": meta,
		"spec":     map[string]any{"nodeName": node, "volumes": volumes},
	}
}

var (
	pvcVols = []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": "c"}}}
	emptyV  = []any{map[string]any{"name": "d", "emptyDir": map[string]any{}}}
	ctrl    = []any{map[string]any{"kind": "ReplicaSet", "controller": true}}
	nonCtrl = []any{map[string]any{"kind": "ReplicaSet", "controller": false}}
)

func TestPolicyManifestSettings(t *testing.T) {
	b, err := os.ReadFile(policyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"failurePolicy: Fail", "validationActions: [Deny]", "name: fileblock-node-pod-delete"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: missing %q", policyFile, want)
		}
	}
}

func TestPolicyCEL(t *testing.T) {
	b, err := os.ReadFile(policyFile)
	if err != nil {
		t.Fatal(err)
	}
	exprs := foldedExpressions(t, string(b))
	if len(exprs) != 4 {
		t.Fatalf("found %d expressions, want 4 (1 matchCondition + 3 validations)", len(exprs))
	}
	env := celEnv(t)
	match, nodeV, ownerV, pvcV := exprs[0], exprs[1], exprs[2], exprs[3]

	cases := []struct {
		name                string
		req                 map[string]any
		obj                 map[string]any
		node, owner, volume bool
	}{
		{"allowed", request(nodeSA, "node-a"), pod("node-a", ctrl, pvcVols), true, true, true},
		{"other node", request(nodeSA, "node-a"), pod("node-b", ctrl, pvcVols), false, true, true},
		{"unowned", request(nodeSA, "node-a"), pod("node-a", nil, pvcVols), true, false, true},
		{"owner not controller", request(nodeSA, "node-a"), pod("node-a", nonCtrl, pvcVols), true, false, true},
		{"no PVC", request(nodeSA, "node-a"), pod("node-a", ctrl, emptyV), true, true, false},
		{"no node-name extra", request(nodeSA, ""), pod("node-a", ctrl, pvcVols), false, true, true},
		{"no extra at all", map[string]any{"userInfo": map[string]any{"username": nodeSA}}, pod("node-a", ctrl, pvcVols), false, true, true},
		{"empty node-name list", map[string]any{"userInfo": map[string]any{"username": nodeSA,
			"extra": map[string]any{"authentication.kubernetes.io/node-name": []any{}}}}, pod("node-a", ctrl, pvcVols), false, true, true},
		{"no nodeName on pod", request(nodeSA, "node-a"), map[string]any{"metadata": map[string]any{"ownerReferences": ctrl},
			"spec": map[string]any{"volumes": pvcVols}}, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := map[string]any{"request": c.req, "oldObject": c.obj}
			for _, v := range []struct {
				label string
				expr  string
				want  bool
			}{{"node", nodeV, c.node}, {"owner", ownerV, c.owner}, {"pvc", pvcV, c.volume}} {
				if got := evalBool(t, env, v.expr, vars); got != v.want {
					t.Errorf("%s validation = %v, want %v", v.label, got, v.want)
				}
			}
			if !evalBool(t, env, match, vars) {
				t.Error("matchCondition = false, want true")
			}
		})
	}

	matchCases := []struct {
		name, user string
		want       bool
	}{
		{"renamed namespace", "system:serviceaccount:storage:fileblock-node", true},
		{"other caller", "system:serviceaccount:kube-system:replicaset-controller", false},
		{"human user", "fileblock-node", false},
	}
	for _, c := range matchCases {
		t.Run(c.name, func(t *testing.T) {
			vars := map[string]any{"request": request(c.user, ""), "oldObject": pod("node-a", ctrl, pvcVols)}
			if got := evalBool(t, env, match, vars); got != c.want {
				t.Errorf("matchCondition = %v, want %v", got, c.want)
			}
		})
	}
}
