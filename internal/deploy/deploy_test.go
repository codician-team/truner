package deploy

import (
	"encoding/json"
	"testing"
)

// The stack policies are AWS's documented ones: deny every update, and allow
// every update during the CLI's own.
func TestStackPolicies(t *testing.T) {
	for policy, effect := range map[string]string{LockPolicy: "Deny", OpenPolicy: "Allow"} {
		var p struct {
			Statement []map[string]string
		}
		if err := json.Unmarshal([]byte(policy), &p); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"Effect": effect, "Action": "Update:*", "Principal": "*", "Resource": "*"}
		if len(p.Statement) != 1 || len(p.Statement[0]) != len(want) {
			t.Fatalf("%s: %v", effect, p)
		}
		for k, v := range want {
			if p.Statement[0][k] != v {
				t.Errorf("%s: %s is %q", effect, k, p.Statement[0][k])
			}
		}
	}
}
