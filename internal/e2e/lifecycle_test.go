package e2e

import "testing"

func TestLifecycleReadinessRequiresEachSurvivor(t *testing.T) {
	raw := []byte(`{"items":[{"metadata":{"name":"a"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"b"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"c"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`)
	if err := lifecycleNodesReady(raw, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][]string{{"a", "c"}, {"missing"}} {
		if err := lifecycleNodesReady(raw, expected); err == nil {
			t.Fatal("unready/missing node accepted")
		}
	}
	if err := lifecycleNodesReady([]byte("invalid"), []string{"a"}); err == nil {
		t.Fatal("malformed node data accepted")
	}
}
