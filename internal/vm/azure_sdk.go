package vm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/v4"
)

const azureOwnerTag = "infestation-mixed-id"

// azureRequest is saved privately before provisioning. These are the same typed
// SDK objects submitted to Azure; the scale set contains secret customData.
type azureRequest struct {
	SecurityGroup armnetwork.SecurityGroup          `json:"securityGroup"`
	Network       armnetwork.VirtualNetwork         `json:"network"`
	ScaleSet      armcompute.VirtualMachineScaleSet `json:"scaleSet"`
}

type azureResource struct{ Name, Type, ID string }
type azureStatus struct{ Code, DisplayStatus string }
type azureInstance struct {
	Name, InstanceID, ProvisioningState string
	Statuses                            []azureStatus
}

type azureAPI interface {
	Group(context.Context, azureScaleSet) (owner string, exists bool, err error)
	CreateGroup(context.Context, azureScaleSet) error
	Provision(context.Context, azureScaleSet, azureRequest, func(string) error) error
	Resources(context.Context, azureScaleSet) ([]azureResource, error)
	Power(context.Context, azureScaleSet, string) error
	Instances(context.Context, azureScaleSet) ([]azureInstance, error)
	DeleteGroup(context.Context, azureScaleSet) error
}

func azureResources(set azureScaleSet, size, publicKey, customData string) azureRequest {
	nsgID := azureNetworkID(set, "networkSecurityGroups", set.Name+"-nsg")
	subnetID := azureNetworkID(set, "virtualNetworks", set.Name+"-vnet") + "/subnets/egress"
	tags := map[string]*string{azureOwnerTag: to.Ptr(set.Ownership)}
	return azureRequest{
		SecurityGroup: armnetwork.SecurityGroup{
			Location: to.Ptr(set.Location), Tags: tags,
			Properties: &armnetwork.SecurityGroupPropertiesFormat{SecurityRules: []*armnetwork.SecurityRule{{
				Name: to.Ptr("deny-all-inbound"), Properties: &armnetwork.SecurityRulePropertiesFormat{
					Priority: to.Ptr(int32(100)), Access: to.Ptr(armnetwork.SecurityRuleAccessDeny), Direction: to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					Protocol: to.Ptr(armnetwork.SecurityRuleProtocolAsterisk), SourceAddressPrefix: to.Ptr("*"), DestinationAddressPrefix: to.Ptr("*"), SourcePortRange: to.Ptr("*"), DestinationPortRange: to.Ptr("*"),
				},
			}}},
		},
		Network: armnetwork.VirtualNetwork{
			Location: to.Ptr(set.Location), Tags: tags,
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{to.Ptr("192.168.240.0/24")}},
				Subnets: []*armnetwork.Subnet{{Name: to.Ptr("egress"), Properties: &armnetwork.SubnetPropertiesFormat{
					AddressPrefix: to.Ptr("192.168.240.0/24"), DefaultOutboundAccess: to.Ptr(true), NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: to.Ptr(nsgID)},
				}}},
			},
		},
		ScaleSet: armcompute.VirtualMachineScaleSet{
			Location: to.Ptr(set.Location), Tags: tags,
			Placement: &armcompute.Placement{ZonePlacementPolicy: to.Ptr(armcompute.ZonePlacementPolicyTypeAuto)},
			SKU:       &armcompute.SKU{Name: to.Ptr(size), Tier: to.Ptr("Standard"), Capacity: to.Ptr(int64(set.Capacity))},
			Properties: &armcompute.VirtualMachineScaleSetProperties{
				OrchestrationMode: to.Ptr(armcompute.OrchestrationModeUniform), Overprovision: to.Ptr(false), SinglePlacementGroup: to.Ptr(false),
				UpgradePolicy: &armcompute.UpgradePolicy{Mode: to.Ptr(armcompute.UpgradeModeManual)},
				VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
					OSProfile: &armcompute.VirtualMachineScaleSetOSProfile{
						ComputerNamePrefix: to.Ptr("node"), AdminUsername: to.Ptr("core"), CustomData: to.Ptr(customData),
						LinuxConfiguration: &armcompute.LinuxConfiguration{DisablePasswordAuthentication: to.Ptr(true), SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{Path: to.Ptr("/home/core/.ssh/authorized_keys"), KeyData: to.Ptr(publicKey)}}}},
					},
					StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
						ImageReference: &armcompute.ImageReference{Publisher: to.Ptr("kinvolk"), Offer: to.Ptr("flatcar-container-linux-corevm-amd64"), SKU: to.Ptr("stable-gen2"), Version: to.Ptr(FlatcarVersion)},
						OSDisk:         &armcompute.VirtualMachineScaleSetOSDisk{CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage), DiskSizeGB: to.Ptr(int32(32)), ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS)}},
					},
					DiagnosticsProfile: &armcompute.DiagnosticsProfile{BootDiagnostics: &armcompute.BootDiagnostics{Enabled: to.Ptr(false)}},
					NetworkProfile: &armcompute.VirtualMachineScaleSetNetworkProfile{NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{{
						Name: to.Ptr("primary"), Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
							Primary: to.Ptr(true), EnableIPForwarding: to.Ptr(true), IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{{
								Name: to.Ptr("primary"), Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{Primary: to.Ptr(true), PrivateIPAddressVersion: to.Ptr(armcompute.IPVersionIPv4), Subnet: &armcompute.APIEntityReference{ID: to.Ptr(subnetID)}},
							}},
						},
					}}},
				},
			},
		},
	}
}
func azureNetworkID(set azureScaleSet, kind, name string) string {
	return "/subscriptions/" + set.Subscription + "/resourceGroups/" + set.Group + "/providers/Microsoft.Network/" + kind + "/" + name
}

type azureSDK struct {
	credential    azcore.TokenCredential
	options       *arm.ClientOptions
	pollFrequency time.Duration
}

func newAzureClient() (azureAPI, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, azureError("configure Azure credentials", err)
	}
	return &azureSDK{credential: credential}, nil
}

// SDK errors can contain response bodies or authentication URLs. Keep those out
// of CLI output while preserving the original error for errors.Is/As.
type azureOperationError struct {
	operation string
	cause     error
}

func (e *azureOperationError) Error() string {
	var response *azcore.ResponseError
	if errors.As(e.cause, &response) {
		return fmt.Sprintf("%s: Azure HTTP %d", e.operation, response.StatusCode)
	}
	if errors.Is(e.cause, context.Canceled) {
		return e.operation + ": canceled"
	}
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return e.operation + ": deadline exceeded"
	}
	return e.operation + " failed; check Azure credentials, permissions and connectivity"
}
func (e *azureOperationError) Unwrap() error { return e.cause }
func azureError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &azureOperationError{operation: operation, cause: err}
}
func completeAzure[T any](ctx context.Context, operation string, poller *runtime.Poller[T], err error, frequency time.Duration) error {
	if err != nil {
		return azureError(operation, err)
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: frequency})
	return azureError(operation, err)
}
func (c *azureSDK) Group(ctx context.Context, set azureScaleSet) (string, bool, error) {
	client, err := armresources.NewResourceGroupsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return "", false, azureError("create resource-group client", err)
	}
	group, err := client.Get(ctx, set.Group, nil)
	var response *azcore.ResponseError
	if errors.As(err, &response) && response.StatusCode == 404 {
		return "", false, nil
	}
	if err != nil {
		return "", false, azureError("read Azure resource group", err)
	}
	return azureValue(group.Tags[azureOwnerTag]), true, nil
}
func (c *azureSDK) CreateGroup(ctx context.Context, set azureScaleSet) error {
	client, err := armresources.NewResourceGroupsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return azureError("create resource-group client", err)
	}
	_, err = client.CreateOrUpdate(ctx, set.Group, armresources.ResourceGroup{Location: to.Ptr(set.Location), Tags: map[string]*string{azureOwnerTag: to.Ptr(set.Ownership)}}, nil)
	return azureError("create Azure resource group", err)
}
func (c *azureSDK) Provision(ctx context.Context, set azureScaleSet, request azureRequest, stage func(string) error) error {
	factory, err := armnetwork.NewClientFactory(set.Subscription, c.credential, c.options)
	if err != nil {
		return azureError("create networking clients", err)
	}
	if err := stage("Creating network security group"); err != nil {
		return err
	}
	nsg, err := factory.NewSecurityGroupsClient().BeginCreateOrUpdate(ctx, set.Group, set.Name+"-nsg", request.SecurityGroup, nil)
	if err := completeAzure(ctx, "create Azure NSG", nsg, err, c.pollFrequency); err != nil {
		return err
	}
	if err := stage("Creating VNet and subnet"); err != nil {
		return err
	}
	vnet, err := factory.NewVirtualNetworksClient().BeginCreateOrUpdate(ctx, set.Group, set.Name+"-vnet", request.Network, nil)
	if err := completeAzure(ctx, "create Azure VNet", vnet, err, c.pollFrequency); err != nil {
		return err
	}
	compute, err := armcompute.NewVirtualMachineScaleSetsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return azureError("create VMSS client", err)
	}
	if err := stage("Creating scale set"); err != nil {
		return err
	}
	vmss, err := compute.BeginCreateOrUpdate(ctx, set.Group, set.Name, request.ScaleSet, nil)
	if err := completeAzure(ctx, "create Azure VMSS", vmss, err, c.pollFrequency); err != nil {
		return err
	}
	return stage("Provisioning completed")
}
func (c *azureSDK) Resources(ctx context.Context, set azureScaleSet) ([]azureResource, error) {
	client, err := armresources.NewClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return nil, azureError("create resources client", err)
	}
	results := []azureResource{}
	pager := client.NewListByResourceGroupPager(set.Group, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureError("list Azure resources", err)
		}
		for _, r := range page.Value {
			results = append(results, azureResource{Name: azureValue(r.Name), Type: azureValue(r.Type), ID: azureValue(r.ID)})
		}
	}
	return results, nil
}
func (c *azureSDK) Power(ctx context.Context, set azureScaleSet, action string) error {
	client, err := armcompute.NewVirtualMachineScaleSetsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return azureError("create VMSS client", err)
	}
	switch action {
	case "start":
		poller, err := client.BeginStart(ctx, set.Group, set.Name, nil)
		return completeAzure(ctx, "start Azure VMSS", poller, err, c.pollFrequency)
	case "stop":
		poller, err := client.BeginDeallocate(ctx, set.Group, set.Name, nil)
		return completeAzure(ctx, "deallocate Azure VMSS", poller, err, c.pollFrequency)
	default:
		return fmt.Errorf("unknown Azure power action")
	}
}
func (c *azureSDK) Instances(ctx context.Context, set azureScaleSet) ([]azureInstance, error) {
	client, err := armcompute.NewVirtualMachineScaleSetVMsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return nil, azureError("create VMSS instances client", err)
	}
	results := []azureInstance{}
	pager := client.NewListPager(set.Group, set.Name, &armcompute.VirtualMachineScaleSetVMsClientListOptions{Expand: to.Ptr("instanceView")})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureError("list Azure VMSS instances", err)
		}
		for _, vm := range page.Value {
			instance := azureInstance{Name: azureValue(vm.Name), InstanceID: azureValue(vm.InstanceID), Statuses: []azureStatus{}}
			if vm.Properties != nil {
				instance.ProvisioningState = azureValue(vm.Properties.ProvisioningState)
				if vm.Properties.InstanceView != nil {
					for _, s := range vm.Properties.InstanceView.Statuses {
						instance.Statuses = append(instance.Statuses, azureStatus{Code: azureValue(s.Code), DisplayStatus: azureValue(s.DisplayStatus)})
					}
				}
			}
			results = append(results, instance)
		}
	}
	return results, nil
}
func (c *azureSDK) DeleteGroup(ctx context.Context, set azureScaleSet) error {
	client, err := armresources.NewResourceGroupsClient(set.Subscription, c.credential, c.options)
	if err != nil {
		return azureError("create resource-group client", err)
	}
	poller, err := client.BeginDelete(ctx, set.Group, nil)
	return completeAzure(ctx, "delete Azure resource group", poller, err, c.pollFrequency)
}

func azureValue[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}
