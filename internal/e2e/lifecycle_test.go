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

func TestLifecycleUsesOneGroupPerComputeHost(t *testing.T) {
	for _, ssh := range []bool{false, true} {
		plans, group, instance := lifecycleGroups("local", "remote", ssh)
		total, initializers := 0, 0
		for _, p := range plans {
			total += p.size
			if p.founding {
				initializers++
			}
		}
		if total != 3 || initializers != 1 {
			t.Fatal("wrong cluster size or founding authority")
		}
		if ssh {
			if len(plans) != 2 || plans[0].size != 2 || plans[1].size != 1 || group != "remote" || instance != "0" {
				t.Fatal("wrong split-host layout")
			}
		} else if len(plans) != 1 || plans[0].size != 3 || group != "local" || instance != "2" {
			t.Fatal("wrong single-host layout")
		}
	}
}
