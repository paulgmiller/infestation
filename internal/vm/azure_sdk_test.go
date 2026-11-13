package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type azureTestCredential struct{}

func (azureTestCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type azureTransport func(*http.Request) (*http.Response, error)

func (f azureTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }
func sdkResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func testAzureSDK(transport azureTransport) *azureSDK {
	return &azureSDK{credential: azureTestCredential{}, pollFrequency: time.Second, options: &arm.ClientOptions{DisableRPRegistration: true, ClientOptions: policy.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}}}}
}
func sdkSet() azureScaleSet {
	return azureScaleSet{Subscription: "sub", Group: "pool-rg", Name: "pool", Ownership: "pool", Location: "westus2", Capacity: 2}
}

func TestSDKTypedProvisionOrdersDependenciesAndPreservesPayload(t *testing.T) {
	set := sdkSet()
	steps := []string{}
	client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
		if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/subscriptions/sub/resourceGroups/pool-rg/providers/") {
			t.Fatalf("unexpected mutation scope: %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(r.URL.Path, "networkSecurityGroups/pool-nsg"):
			steps = append(steps, "nsg")
			if !strings.Contains(string(body), `"access":"Deny"`) {
				t.Fatal("lost security rule")
			}
		case strings.Contains(r.URL.Path, "virtualNetworks/pool-vnet"):
			if strings.Join(steps, ",") != "nsg" {
				t.Fatal("network before NSG completion")
			}
			steps = append(steps, "vnet")
			if !strings.Contains(string(body), azureNetworkID(set, "networkSecurityGroups", "pool-nsg")) {
				t.Fatal("wrong NSG reference")
			}
		case strings.Contains(r.URL.Path, "virtualMachineScaleSets/pool"):
			if strings.Join(steps, ",") != "nsg,vnet" {
				t.Fatal("VMSS before network completion")
			}
			steps = append(steps, "vmss")
			if !strings.Contains(string(body), `"customData":"private-profile"`) || !strings.Contains(string(body), `"zonePlacementPolicy":"Auto"`) || !strings.Contains(string(body), `"capacity":2`) {
				t.Fatal("lost VMSS settings")
			}
			if strings.Contains(string(body), "parameters(") {
				t.Fatal("ARM expression in typed payload")
			}
		default:
			t.Fatalf("unexpected resource %s", r.URL)
		}
		return sdkResponse(r, 201, `{"properties":{"provisioningState":"Succeeded"}}`), nil
	})
	request := azureResources(set, "Standard_B2s", "ssh-rsa disposable", "private-profile")
	if err := client.Provision(context.Background(), set, request, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "nsg,vnet,vmss" {
		t.Fatal("incomplete provisioning", steps)
	}
}

func TestSDKProvisionFailureStopsBeforeDependentsAndRedactsResponse(t *testing.T) {
	calls := 0
	client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.URL.Path, "networkSecurityGroups/") {
			t.Fatal("continued after NSG failure")
		}
		return sdkResponse(r, 403, `{"error":{"code":"Forbidden","message":"SECRET-private-profile"}}`), nil
	})
	err := client.Provision(context.Background(), sdkSet(), azureResources(sdkSet(), "sku", "key", "SECRET-private-profile"), func(string) error { return nil })
	var response *azcore.ResponseError
	if err == nil || strings.Contains(err.Error(), "SECRET") || !errors.As(err, &response) || calls != 1 {
		t.Fatalf("failure not safely propagated: %v", err)
	}
}

func TestSDKPowerAndRemovalPollToCompletionAndHonorCancellation(t *testing.T) {
	for _, action := range []string{"start", "stop", "remove", "cancel"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := []string{}
			client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if strings.HasPrefix(r.URL.Path, "/operations/") {
					if r.Method != "GET" {
						t.Fatal("poll is not GET")
					}
					return sdkResponse(r, 200, `{"status":"Succeeded"}`), nil
				}
				wantMethod, wantPath := "POST", "/subscriptions/sub/resourceGroups/pool-rg/providers/Microsoft.Compute/virtualMachineScaleSets/pool/start"
				if action == "stop" {
					wantPath = strings.TrimSuffix(wantPath, "start") + "deallocate"
				}
				if action == "remove" {
					wantMethod = "DELETE"
					wantPath = "/subscriptions/sub/resourceGroups/pool-rg"
				}
				if r.Method != wantMethod || !strings.EqualFold(r.URL.Path, wantPath) || r.URL.Query().Has("instanceIds") {
					t.Fatalf("wrong target: %s %s", r.Method, r.URL)
				}
				res := sdkResponse(r, 202, "")
				res.Header.Set("Azure-AsyncOperation", "https://management.azure.com/operations/test")
				if action == "cancel" {
					cancel()
				}
				return res, nil
			})
			var err error
			if action == "remove" {
				err = client.DeleteGroup(ctx, sdkSet())
			} else {
				power := action
				if power == "cancel" {
					power = "start"
				}
				err = client.Power(ctx, sdkSet(), power)
			}
			if action == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			} else if err != nil || len(calls) < 2 {
				t.Fatalf("did not wait for completion: %v %v", err, calls)
			}
		})
	}
}

func TestSDKGroupOwnershipAndAbsence(t *testing.T) {
	for _, status := range []int{200, 404, 403} {
		client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || !strings.EqualFold(r.URL.Path, "/subscriptions/sub/resourceGroups/pool-rg") {
				t.Fatal("wrong ownership lookup")
			}
			return sdkResponse(r, status, `{"tags":{"infestation-mixed-id":"pool"}}`), nil
		})
		owner, exists, err := client.Group(context.Background(), sdkSet())
		if status == 200 && (err != nil || !exists || owner != "pool") {
			t.Fatal("lost ownership", err)
		}
		if status == 404 && (err != nil || exists) {
			t.Fatal("missing group not handled", err)
		}
		if status == 403 && err == nil {
			t.Fatal("authorization failure treated as absence")
		}
	}
	client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || !strings.Contains(string(body), `"infestation-mixed-id":"pool"`) || !strings.Contains(string(body), `"location":"westus2"`) {
			t.Fatal("group ownership/location not set")
		}
		return sdkResponse(r, 201, `{"location":"westus2"}`), nil
	})
	if err := client.CreateGroup(context.Background(), sdkSet()); err != nil {
		t.Fatal(err)
	}
}

func TestSDKStatusAndResourceListsReadEveryPageWithoutSecrets(t *testing.T) {
	for _, kind := range []string{"resources", "instances"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			client := testAzureSDK(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" {
					t.Fatal("list mutated resources")
				}
				if kind == "instances" && calls == 1 && r.URL.Query().Get("$expand") != "instanceView" {
					t.Fatal("missing instance view")
				}
				item := `{"name":"node","type":"compute","id":"id","instanceId":"0","properties":{"provisioningState":"Succeeded","osProfile":{"customData":"SECRET-profile"},"instanceView":{"statuses":[{"code":"PowerState/running","displayStatus":"VM running"}]}}}`
				next := ""
				if calls == 1 {
					next = `,"nextLink":"https://management.azure.com/subscriptions/sub/resourceGroups/pool-rg/next"`
				}
				return sdkResponse(r, 200, `{"value":[`+item+`]`+next+`}`), nil
			})
			var data []byte
			if kind == "resources" {
				v, err := client.Resources(context.Background(), sdkSet())
				if err != nil {
					t.Fatal(err)
				}
				if len(v) != 2 {
					t.Fatal("missing resource page")
				}
				data, _ = json.Marshal(v)
			} else {
				v, err := client.Instances(context.Background(), sdkSet())
				if err != nil {
					t.Fatal(err)
				}
				if len(v) != 2 || v[0].Statuses[0].Code != "PowerState/running" {
					t.Fatal("missing instance page/status")
				}
				data, _ = json.Marshal(v)
			}
			if calls != 2 || strings.Contains(string(data), "SECRET") {
				t.Fatal("list omitted pages or disclosed profile")
			}
		})
	}
}
