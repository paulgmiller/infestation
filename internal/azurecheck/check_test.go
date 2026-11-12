package azurecheck

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/executil"
)

func TestCycleAlwaysTearsDownAndPreservesBothErrors(t *testing.T) {
	provisionErr, testErr, cleanupErr := errors.New("provision"), errors.New("test"), errors.New("cleanup")
	for _, tc := range []struct {
		name                     string
		provision, test, cleanup error
		cancel                   bool
	}{
		{name: "success"}, {name: "provision fails", provision: provisionErr}, {name: "test fails", test: testErr}, {name: "cleanup fails", cleanup: cleanupErr}, {name: "both fail", test: testErr, cleanup: cleanupErr}, {name: "cancelled", provision: context.Canceled, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			err := cycle(ctx, func(context.Context) error {
				calls = append(calls, "provision")
				if tc.cancel {
					cancel()
				}
				return tc.provision
			}, func(context.Context) error { calls = append(calls, "smoke"); return tc.test }, func(clean context.Context) error {
				calls = append(calls, "teardown")
				if clean.Err() != nil {
					t.Fatal("cancelled parent skipped cleanup")
				}
				if _, ok := clean.Deadline(); !ok {
					t.Fatal("cleanup must be bounded")
				}
				return tc.cleanup
			})
			want := []string{"provision", "smoke", "teardown"}
			if tc.provision != nil {
				want = []string{"provision", "teardown"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls)
			}
			for _, e := range []error{tc.provision, tc.test, tc.cleanup} {
				if e != nil && !errors.Is(err, e) {
					t.Fatalf("lost %v in %v", e, err)
				}
			}
			if tc.provision == nil && tc.test == nil && tc.cleanup == nil && err != nil {
				t.Fatal(err)
			}
		})
	}
}

type runnerFunc func(context.Context, string, ...string) executil.Result

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) executil.Result {
	return f(ctx, name, args...)
}

func TestCleanupChecksOwnershipAndVerifiesAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, tags, lock    string
		absent, deleteFails bool
		wantErr             bool
	}{
		{name: "owned", tags: `{"infestation-check-id":"our-id"}`, lock: "/subscriptions/sub/resourceGroups/test-rg/providers/Microsoft.Authorization/locks/retain-artifacts"},
		{name: "already absent", absent: true},
		{name: "foreign", tags: `{"infestation-check-id":"someone-else"}`, wantErr: true},
		{name: "untagged", tags: `{}`, wantErr: true},
		{name: "foreign lock", tags: `{"infestation-check-id":"our-id"}`, lock: "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Authorization/locks/lock", wantErr: true},
		{name: "delete fails", tags: `{"infestation-check-id":"our-id"}`, deleteFails: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted, lockDeleted, existsCalls := false, false, 0
			r := runnerFunc(func(_ context.Context, name string, args ...string) executil.Result {
				if name != "az" {
					t.Fatal(name)
				}
				if !strings.Contains(strings.Join(args, " "), "--subscription sub") {
					t.Fatal("subscription must be pinned")
				}
				switch args[0] + " " + args[1] {
				case "group exists":
					existsCalls++
					if tc.absent || deleted {
						return executil.Result{Stdout: "false"}
					}
					return executil.Result{Stdout: "true"}
				case "group show":
					return executil.Result{Stdout: tc.tags}
				case "lock list":
					if tc.lock == "" {
						return executil.Result{Stdout: "[]"}
					}
					return executil.Result{Stdout: `[{"id":"` + tc.lock + `"}]`}
				case "lock delete":
					lockDeleted = true
					return executil.Result{}
				case "group delete":
					if tc.lock != "" && !lockDeleted {
						t.Fatal("delete attempted before unlocking")
					}
					if tc.deleteFails {
						return executil.Result{Code: 1, Stderr: "failed"}
					}
					deleted = true
					return executil.Result{}
				}
				t.Fatal(args)
				return executil.Result{}
			})
			err := cleanup(context.Background(), r, "sub", "test-rg", "our-id")
			if (err != nil) != tc.wantErr {
				t.Fatalf("%v", err)
			}
			if tc.wantErr && deleted {
				t.Fatal("unsafe deletion")
			}
			if !tc.wantErr && !tc.absent && (!deleted || existsCalls != 2) {
				t.Fatal("deletion was not verified")
			}
		})
	}
}
