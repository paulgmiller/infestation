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

	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type azureNode struct {
	Subscription, Group, Node, Location, Ownership string
	PublicKey                                      string
}

func loadAzureNode(dir string) (azureNode, bool) {
	var node azureNode
	err := fsutil.ReadJSON(filepath.Join(dir, "azure-node.json"), &node)
	return node, err == nil
}

type azureScaleSet struct {
	Subscription, Group, Name, Location, Ownership string
	Capacity                                       int
}

// azureScaleSetTemplate deliberately supplies no public ingress, load balancer,
// artifact storage, managed identity, private DNS or guest command extension.
func azureScaleSetTemplate(set azureScaleSet, size, publicKey string) map[string]any {
	networkID := func(kind, name string) string {
		return "[resourceId('Microsoft.Network/" + kind + "', '" + name + "')]"
	}
	nsg, vnet := set.Name+"-nsg", set.Name+"-vnet"
	subnetID := "[resourceId('Microsoft.Network/virtualNetworks/subnets', '" + vnet + "', 'egress')]"
	network := func(kind, name string, properties map[string]any, deps ...string) map[string]any {
		r := map[string]any{"type": "Microsoft.Network/" + kind, "apiVersion": "2024-05-01", "name": name, "location": set.Location, "properties": properties}
		if len(deps) > 0 {
			r["dependsOn"] = deps
		}
		return r
	}
	profile := map[string]any{
		"osProfile":          map[string]any{"computerNamePrefix": "node", "adminUsername": "core", "customData": "[parameters('customData')]", "linuxConfiguration": map[string]any{"disablePasswordAuthentication": true, "ssh": map[string]any{"publicKeys": []any{map[string]any{"path": "/home/core/.ssh/authorized_keys", "keyData": publicKey}}}}},
		"storageProfile":     map[string]any{"imageReference": map[string]any{"publisher": "kinvolk", "offer": "flatcar-container-linux-corevm-amd64", "sku": "stable-gen2", "version": FlatcarVersion}, "osDisk": map[string]any{"createOption": "FromImage", "diskSizeGB": 32, "managedDisk": map[string]any{"storageAccountType": "StandardSSD_LRS"}}},
		"diagnosticsProfile": map[string]any{"bootDiagnostics": map[string]any{"enabled": false}},
		"networkProfile":     map[string]any{"networkInterfaceConfigurations": []any{map[string]any{"name": "primary", "properties": map[string]any{"primary": true, "enableIPForwarding": true, "ipConfigurations": []any{map[string]any{"name": "primary", "properties": map[string]any{"primary": true, "privateIPAddressVersion": "IPv4", "subnet": map[string]any{"id": subnetID}}}}}}}},
	}
	resources := []any{
		network("networkSecurityGroups", nsg, map[string]any{"securityRules": []any{map[string]any{"name": "deny-all-inbound", "properties": map[string]any{"priority": 100, "access": "Deny", "direction": "Inbound", "protocol": "*", "sourceAddressPrefix": "*", "destinationAddressPrefix": "*", "sourcePortRange": "*", "destinationPortRange": "*"}}}}),
		network("virtualNetworks", vnet, map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"192.168.240.0/24"}}, "subnets": []any{map[string]any{"name": "egress", "properties": map[string]any{"addressPrefix": "192.168.240.0/24", "defaultOutboundAccess": true, "networkSecurityGroup": map[string]any{"id": networkID("networkSecurityGroups", nsg)}}}}}, networkID("networkSecurityGroups", nsg)),
		map[string]any{"type": "Microsoft.Compute/virtualMachineScaleSets", "apiVersion": "2024-07-01", "name": set.Name, "location": set.Location, "dependsOn": []string{networkID("virtualNetworks", vnet)}, "sku": map[string]any{"name": size, "tier": "Standard", "capacity": set.Capacity}, "properties": map[string]any{"orchestrationMode": "Uniform", "overprovision": false, "singlePlacementGroup": true, "upgradePolicy": map[string]any{"mode": "Manual"}, "virtualMachineProfile": profile}},
	}
	return map[string]any{"$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#", "contentVersion": "1.0.0.0", "parameters": map[string]any{"customData": map[string]any{"type": "secureString"}}, "resources": resources}
}

func createAzureNodes(ctx context.Context, o ClusterOptions, dir string, inventory []cluster.Node, out io.Writer, runner executil.Runner) error {
	if len(inventory) == 0 {
		return fmt.Errorf("Azure creation requires nodes")
	}
	account := runner.Run(ctx, "az", "account", "show", "--query", "id", "-o", "tsv")
	if err := account.Err("read Azure subscription"); err != nil {
		return err
	}
	subscription := strings.TrimSpace(account.Stdout)
	if subscription == "" {
		return fmt.Errorf("Azure subscription is missing")
	}
	set := azureScaleSet{Subscription: subscription, Group: o.Name + "-rg", Name: o.Name, Location: o.AzureLocation, Ownership: o.Name, Capacity: len(inventory)}
	exists := runner.Run(ctx, "az", "group", "exists", "--subscription", subscription, "--name", set.Group, "-o", "tsv")
	if err := exists.Err("check Azure group ownership"); err != nil {
		return err
	}
	if strings.TrimSpace(exists.Stdout) != "false" {
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
	template := filepath.Join(dir, "azure-template.json")
	parameterPath := filepath.Join(dir, "azure-parameters.json")
	if err := fsutil.WriteJSON(template, azureScaleSetTemplate(set, o.AzureVMSize, publicKey)); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(parameterPath, map[string]any{"customData": map[string]string{"value": base64.StdEncoding.EncodeToString(raw)}}); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "azure-scaleset.json"), set); err != nil {
		return err
	}
	group := runner.Run(ctx, "az", "group", "create", "--subscription", subscription, "--name", set.Group, "--location", set.Location, "--tags", "infestation-mixed-id="+set.Ownership, "--query", "properties.provisioningState", "-o", "tsv")
	if err := group.Err("create Azure resource group"); err != nil {
		return err
	}
	fmt.Fprintf(out, "Creating Azure VMSS %s with %d instances in %s\n", set.Name, set.Capacity, set.Location)
	deployed := runner.Run(ctx, "az", "deployment", "group", "create", "--subscription", subscription, "--resource-group", set.Group, "--name", "nodes", "--template-file", template, "--parameters", "@"+parameterPath, "--query", "properties.provisioningState", "-o", "tsv")
	if err := deployed.Err("deploy Azure scale set"); err != nil {
		return fmt.Errorf("%w; owned group %s retained for diagnosis", err, set.Group)
	}
	resources := runner.Run(ctx, "az", "resource", "list", "--subscription", subscription, "--resource-group", set.Group, "--query", "[].{name:name,type:type}", "-o", "json")
	if err := resources.Err("record Azure resources"); err != nil {
		return err
	}
	var evidence any
	if err := json.Unmarshal([]byte(resources.Stdout), &evidence); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "azure-resources.json"), evidence)
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

// AzureAction operates only the complete owned Azure inventory, including older
// single-VM records. It never creates missing machines as a power-operation retry.
func AzureAction(ctx context.Context, o Options, action string, out io.Writer) error {
	return azureActions(ctx, o, action, out, executil.OSRunner{})
}
func azureActions(ctx context.Context, o Options, action string, out io.Writer, runner executil.Runner) error {
	if action != "start" && action != "stop" && action != "status" {
		return fmt.Errorf("unknown Azure VM action")
	}
	dir, _, err := loadCluster(o)
	if err != nil {
		return err
	}
	var set azureScaleSet
	err = fsutil.ReadJSON(filepath.Join(dir, "azure-scaleset.json"), &set)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Keep earlier single-VM groups controllable without migrating their resources.
		var node azureNode
		if err := fsutil.ReadJSON(filepath.Join(dir, "azure-node.json"), &node); err != nil {
			return err
		}
		if node.Ownership != o.Name || node.Group != o.Name+"-rg" || node.Subscription == "" {
			return fmt.Errorf("Azure VM ownership mismatch")
		}
		return runAzurePower(ctx, runner, "vm", node.Subscription, node.Group, node.Node, action, out)
	}
	if set.Name != o.Name || set.Group != o.Name+"-rg" || set.Ownership != o.Name || set.Subscription == "" || set.Capacity < 1 {
		return fmt.Errorf("Azure scale-set ownership mismatch")
	}
	owner := runner.Run(ctx, "az", "group", "show", "--subscription", set.Subscription, "--name", set.Group, "--query", "tags.\"infestation-mixed-id\"", "-o", "tsv")
	if err := owner.Err("verify Azure resource-group ownership"); err != nil {
		return err
	}
	if strings.TrimSpace(owner.Stdout) != o.Name {
		return fmt.Errorf("Azure resource-group ownership mismatch")
	}
	return runAzurePower(ctx, runner, "vmss", set.Subscription, set.Group, set.Name, action, out)
}
func runAzurePower(ctx context.Context, runner executil.Runner, kind, subscription, group, name, action string, out io.Writer) error {
	verb := action
	if action == "stop" {
		verb = "deallocate"
	}
	if action == "status" {
		verb = "get-instance-view"
		if kind == "vmss" {
			verb = "list-instances"
		}
	}
	args := []string{kind, verb, "--subscription", subscription, "--resource-group", group, "--name", name}
	if action == "status" {
		if kind == "vmss" {
			args = append(args, "--expand", "instanceView")
		} else {
			args = append(args, "--query", "instanceView.statuses")
		}
		args = append(args, "-o", "json")
	}
	result := runner.Run(ctx, "az", args...)
	if err := result.Err("Azure " + kind + " " + verb); err != nil {
		return err
	}
	fmt.Fprintln(out, name, "Azure", kind, verb, "completed")
	if action == "status" {
		fmt.Fprintln(out, result.Stdout)
	}
	return nil
}
