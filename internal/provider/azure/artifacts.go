package azure

import (
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
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

func artifactStorage(ctx *pulumi.Context, group pulumi.StringInput, location pulumi.StringInput) error {
	var identity struct {
		ObjectID       string `pulumi:"objectId"`
		SubscriptionID string `pulumi:"subscriptionId"`
	}
	if err := ctx.Invoke("azure-native:authorization:getClientConfig", nil, &identity, pulumi.Version("3.29.0")); err != nil {
		return err
	}
	if identity.ObjectID == "" || identity.SubscriptionID == "" {
		return fmt.Errorf("Azure provider identity is required for private artifact storage access")
	}
	accountName := group.ToStringOutput().ApplyT(func(name string) string {
		sum := sha256.Sum256([]byte(identity.SubscriptionID + "/" + name + "/artifacts"))
		return fmt.Sprintf("inf%x", sum[:10])
	}).(pulumi.StringOutput)
	account, err := artifactResourceNew(ctx, "storage:StorageAccount", "artifacts", pulumi.Map{
		"accountName": accountName, "resourceGroupName": group, "location": location,
		"kind": pulumi.String("StorageV2"), "sku": pulumi.Map{"name": pulumi.String("Standard_RAGZRS")},
		"allowBlobPublicAccess": pulumi.Bool(false), "allowSharedKeyAccess": pulumi.Bool(false),
		"defaultToOAuthAuthentication": pulumi.Bool(true), "enableHttpsTrafficOnly": pulumi.Bool(true),
		"minimumTlsVersion": pulumi.String("TLS1_2"),
	}, pulumi.Protect(true))
	if err != nil {
		return err
	}
	container, err := artifactResourceNew(ctx, "storage:BlobContainer", "access-artifacts", pulumi.Map{
		"accountName": accountName, "resourceGroupName": group,
		"containerName": pulumi.String("access"), "publicAccess": pulumi.String("None"),
	}, pulumi.DependsOn([]pulumi.Resource{account}))
	if err != nil {
		return err
	}
	// The deployment identity publishes and fetches. Additional operators can be
	// assigned Storage Blob Data Reader on this container through Azure RBAC.
	role := "ba92f5b4-2d11-453d-a403-e96b0029c9fe" // Storage Blob Data Contributor
	assignment := container.ID().ToStringOutput().ApplyT(func(id string) string {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(id+"/"+identity.ObjectID+"/"+role)).String()
	}).(pulumi.StringOutput)
	_, err = artifactResourceNew(ctx, "authorization:RoleAssignment", "artifact-publisher", pulumi.Map{
		"roleAssignmentName": assignment, "scope": container.ID(), "principalId": pulumi.String(identity.ObjectID),
		"roleDefinitionId": pulumi.String("/subscriptions/" + identity.SubscriptionID + "/providers/Microsoft.Authorization/roleDefinitions/" + role),
	})
	if err != nil {
		return err
	}
	_, err = artifactResourceNew(ctx, "authorization:ManagementLockAtResourceLevel", "protect-artifacts", pulumi.Map{
		"resourceGroupName": group, "resourceProviderNamespace": pulumi.String("Microsoft.Storage"),
		"parentResourcePath": pulumi.String(""), "resourceType": pulumi.String("storageAccounts"),
		"resourceName": accountName, "lockName": pulumi.String("retain-artifacts"),
		"level": pulumi.String("CanNotDelete"), "notes": pulumi.String("Remove explicitly before deleting this cluster resource group and its recovery artifacts."),
	}, pulumi.DependsOn([]pulumi.Resource{account}), pulumi.Protect(true))
	if err != nil {
		return err
	}
	ctx.Export("kubeconfigBlob", pulumi.Map{
		"account": accountName, "container": pulumi.String("access"), "blob": pulumi.String("kubeconfig.json"),
		"subscriptionId": pulumi.String(identity.SubscriptionID),
	})
	ctx.Export("artifactContainerID", container.ID())
	return nil
}
