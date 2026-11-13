package vm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

type azureScaleSet struct {
	Subscription, Group, Name, Location, Ownership string
	Capacity                                       int
}

func createAzureNodes(ctx context.Context, o ClusterOptions, dir string, inventory []cluster.Node, out io.Writer, client azureAPI) error {
	if len(inventory) == 0 {
		return fmt.Errorf("Azure creation requires nodes")
	}
	subscription, err := azureSubscription(o.AzureSubscription)
	if err != nil {
		return err
	}
	set := azureScaleSet{Subscription: subscription, Group: o.Name + "-rg", Name: o.Name, Location: o.AzureLocation, Ownership: o.Name, Capacity: len(inventory)}
	_, exists, err := client.Group(ctx, set)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("refusing existing Azure resource group %s", set.Group)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "output", inventory[0].Name+".ign"))
	if err != nil {
		return err
	}
	if len(raw) > 65535 {
		return fmt.Errorf("Azure shared Ignition exceeds customData limit")
	}
	publicKey, err := disposableAzurePublicKey()
	if err != nil {
		return err
	}
	plan := azureResources(set, o.AzureVMSize, publicKey, base64.StdEncoding.EncodeToString(raw))
	if err := fsutil.WriteJSON(filepath.Join(dir, "azure-request.json"), plan); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "azure-scaleset.json"), set); err != nil {
		return err
	}
	fmt.Fprintf(out, "Creating Azure resource group %s in subscription %s (%s)\n", set.Group, set.Subscription, set.Location)
	if err := client.CreateGroup(ctx, set); err != nil {
		return err
	}
	fmt.Fprintf(out, "Creating Azure VMSS %s with %d instances in %s\n", set.Name, set.Capacity, set.Location)
	if err := client.Provision(ctx, set, plan, func(stage string) error {
		fmt.Fprintln(out, stage)
		return fsutil.WriteJSON(filepath.Join(dir, "azure-progress.json"), map[string]string{"stage": stage})
	}); err != nil {
		return fmt.Errorf("%w; owned group %s retained for diagnosis", err, set.Group)
	}
	resources, err := client.Resources(ctx, set)
	if err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "azure-resources.json"), resources)
}

func disposableAzurePublicKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	// Never serialize the private half: this satisfies Azure's OS profile check,
	// and is not a usable management credential. Standard sshd stays masked.
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}

// AzureAction operates on the whole owned scale set, without recreating missing VMs.
func AzureAction(ctx context.Context, o Options, action string, out io.Writer) error {
	client, err := newAzureClient()
	if err != nil {
		return err
	}
	return azureActions(ctx, o, action, out, client)
}
func azureActions(ctx context.Context, o Options, action string, out io.Writer, client azureAPI) error {
	if action != "start" && action != "stop" && action != "status" {
		return fmt.Errorf("unknown Azure VM action")
	}
	_, set, err := loadAzureSet(o)
	if err != nil {
		return err
	}
	owner, exists, err := client.Group(ctx, set)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Azure resource group %s does not exist", set.Group)
	}
	if owner != set.Ownership {
		return fmt.Errorf("Azure resource-group ownership mismatch")
	}
	if action == "status" {
		instances, err := client.Instances(ctx, set)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(instances)
	}
	if err := client.Power(ctx, set, action); err != nil {
		return err
	}
	fmt.Fprintln(out, set.Name, "Azure VMSS", action, "completed")
	return nil
}
func loadAzureSet(o Options) (string, azureScaleSet, error) {
	dir, _, err := loadCluster(o)
	var set azureScaleSet
	if err != nil {
		return dir, set, err
	}
	var provider string
	if err := fsutil.ReadJSON(filepath.Join(dir, "provider.json"), &provider); err != nil {
		return dir, set, err
	}
	if provider != "azure" {
		return dir, set, fmt.Errorf("Azure provider ownership mismatch")
	}
	if err := fsutil.ReadJSON(filepath.Join(dir, "azure-scaleset.json"), &set); err != nil {
		return dir, set, err
	}
	if set.Name != o.Name || set.Group != o.Name+"-rg" || set.Ownership != o.Name || set.Subscription == "" || set.Capacity < 1 {
		return dir, set, fmt.Errorf("Azure scale-set ownership mismatch")
	}
	return dir, set, nil
}
func azureSubscription(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return "", fmt.Errorf("supply --subscription or AZURE_SUBSCRIPTION_ID")
	}
	return explicit, nil
}
