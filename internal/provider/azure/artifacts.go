package azure

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"infestation/internal/blobstore"
)

// These few native resource registrations avoid pulling in the large generated
// storage/authorization SDKs. They use the same pinned provider as the VM SDK.
type artifactResource struct{ pulumi.CustomResourceState }

func artifactResourceNew(ctx *pulumi.Context, token, name string, args pulumi.Map, opts ...pulumi.ResourceOption) (*artifactResource, error) {
	r := &artifactResource{}
	opts = append([]pulumi.ResourceOption{pulumi.Version("3.29.0")}, opts...)
	err := ctx.RegisterResource("azure-native:"+token, name, args, r, opts...)
	return r, err
}

type artifactAccess struct {
	Assets, Access *artifactResource
	Subscription   string
}

func artifactStorage(ctx *pulumi.Context, group pulumi.StringInput, location pulumi.StringInput) (*artifactAccess, error) {
	var identity struct {
		ObjectID       string `pulumi:"objectId"`
		SubscriptionID string `pulumi:"subscriptionId"`
	}
	if err := ctx.Invoke("azure-native:authorization:getClientConfig", nil, &identity, pulumi.Version("3.29.0")); err != nil {
		return nil, err
	}
	if identity.ObjectID == "" || identity.SubscriptionID == "" {
		return nil, fmt.Errorf("Azure provider identity is required for private artifact storage access")
	}
	accountName := group.ToStringOutput().ApplyT(func(name string) string {
		return blobstore.AccountName(identity.SubscriptionID, name)
	}).(pulumi.StringOutput)
	account, err := artifactResourceNew(ctx, "storage:StorageAccount", "artifacts", pulumi.Map{
		"accountName": accountName, "resourceGroupName": group, "location": location,
		"kind": pulumi.String("StorageV2"), "sku": pulumi.Map{"name": pulumi.String("Standard_RAGZRS")},
		"allowBlobPublicAccess": pulumi.Bool(false), "allowSharedKeyAccess": pulumi.Bool(false),
		"defaultToOAuthAuthentication": pulumi.Bool(true), "enableHttpsTrafficOnly": pulumi.Bool(true),
		"minimumTlsVersion": pulumi.String("TLS1_2"),
	}, pulumi.Protect(true))
	if err != nil {
		return nil, err
	}
	container, err := artifactResourceNew(ctx, "storage:BlobContainer", "access-artifacts", pulumi.Map{
		"accountName": accountName, "resourceGroupName": group,
		"containerName": pulumi.String("access"), "publicAccess": pulumi.String("None"),
	}, pulumi.DependsOn([]pulumi.Resource{account}))
	if err != nil {
		return nil, err
	}
	assets, err := artifactResourceNew(ctx, "storage:BlobContainer", "bootstrap-assets", pulumi.Map{
		"accountName": accountName, "resourceGroupName": group,
		"containerName": pulumi.String("bootstrap-assets"), "publicAccess": pulumi.String("None"),
	}, pulumi.DependsOn([]pulumi.Resource{account}))
	if err != nil {
		return nil, err
	}
	access := &artifactAccess{Assets: assets, Access: container, Subscription: identity.SubscriptionID}
	if err := access.grant(ctx, "artifact-publisher", container, pulumi.String(identity.ObjectID), true); err != nil {
		return nil, err
	}
	if err := access.grant(ctx, "asset-publisher", assets, pulumi.String(identity.ObjectID), true); err != nil {
		return nil, err
	}
	_, err = artifactResourceNew(ctx, "authorization:ManagementLockAtResourceLevel", "protect-artifacts", pulumi.Map{
		"resourceGroupName": group, "resourceProviderNamespace": pulumi.String("Microsoft.Storage"),
		"parentResourcePath": pulumi.String(""), "resourceType": pulumi.String("storageAccounts"),
		"resourceName": accountName, "lockName": pulumi.String("retain-artifacts"),
		"level": pulumi.String("CanNotDelete"), "notes": pulumi.String("Remove explicitly before deleting this cluster resource group and its recovery artifacts."),
	}, pulumi.DependsOn([]pulumi.Resource{account}), pulumi.Protect(true))
	if err != nil {
		return nil, err
	}
	ctx.Export("kubeconfigBlob", pulumi.Map{
		"account": accountName, "container": pulumi.String("access"), "blob": pulumi.String("kubeconfig.json"),
		"subscriptionId": pulumi.String(identity.SubscriptionID),
	})
	ctx.Export("artifactContainerID", container.ID())
	return access, nil
}

func (a *artifactAccess) grant(ctx *pulumi.Context, name string, container *artifactResource, principal pulumi.StringInput, write bool, servicePrincipal ...bool) error {
	role := "2a2b9908-6ea1-4ae2-8e65-a410df84e7d1" // Storage Blob Data Reader
	if write {
		role = "ba92f5b4-2d11-453d-a403-e96b0029c9fe"
	}
	assignment := pulumi.All(container.ID().ToStringOutput(), principal.ToStringOutput()).ApplyT(func(values []interface{}) string {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(values[0].(string)+"/"+values[1].(string)+"/"+role)).String()
	}).(pulumi.StringOutput)
	args := pulumi.Map{
		"roleAssignmentName": assignment, "scope": container.ID(), "principalId": principal,
		"roleDefinitionId": pulumi.String("/subscriptions/" + a.Subscription + "/providers/Microsoft.Authorization/roleDefinitions/" + role),
	}
	if len(servicePrincipal) > 0 && servicePrincipal[0] {
		args["principalType"] = pulumi.String("ServicePrincipal")
	}
	_, err := artifactResourceNew(ctx, "authorization:RoleAssignment", name, args)
	return err
}
