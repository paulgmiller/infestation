package vm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// azureTemplate intentionally has no artifact storage, managed identity, public
// ingress, load balancer, private DNS or guest command extensions. Static node
// inventory and tailnet DNS work identically on QEMU and Azure.
func azureTemplate(node azureNode, size string) map[string]any {
	resourceID := func(kind, name string) string {
		return "[resourceId('Microsoft.Network/" + kind + "', '" + name + "')]"
	}
	nsg, vnet, nic := node.Node+"-nsg", node.Node+"-vnet", node.Node+"-nic"
	subnetID := "[resourceId('Microsoft.Network/virtualNetworks/subnets', '" + vnet + "', 'egress')]"
	resource := func(kind, name string, properties map[string]any, dependencies ...string) map[string]any {
		r := map[string]any{"type": "Microsoft.Network/" + kind, "apiVersion": "2024-05-01", "name": name, "location": node.Location, "properties": properties}
		if len(dependencies) > 0 {
			r["dependsOn"] = dependencies
		}
		return r
	}
	resources := []any{
		resource("networkSecurityGroups", nsg, map[string]any{"securityRules": []any{map[string]any{"name": "deny-all-inbound", "properties": map[string]any{"priority": 100, "access": "Deny", "direction": "Inbound", "protocol": "*", "sourceAddressPrefix": "*", "destinationAddressPrefix": "*", "sourcePortRange": "*", "destinationPortRange": "*"}}}}),
		resource("virtualNetworks", vnet, map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"192.168.240.0/24"}}, "subnets": []any{map[string]any{"name": "egress", "properties": map[string]any{"addressPrefix": "192.168.240.0/24", "defaultOutboundAccess": true, "networkSecurityGroup": map[string]any{"id": resourceID("networkSecurityGroups", nsg)}}}}}, resourceID("networkSecurityGroups", nsg)),
		resource("networkInterfaces", nic, map[string]any{"enableIPForwarding": true, "ipConfigurations": []any{map[string]any{"name": "primary", "properties": map[string]any{"privateIPAllocationMethod": "Dynamic", "subnet": map[string]any{"id": subnetID}}}}}, resourceID("virtualNetworks", vnet)),
		map[string]any{"type": "Microsoft.Compute/virtualMachines", "apiVersion": "2024-07-01", "name": node.Node, "location": node.Location, "dependsOn": []string{resourceID("networkInterfaces", nic)}, "properties": map[string]any{
			"hardwareProfile":    map[string]any{"vmSize": size},
			"osProfile":          map[string]any{"computerName": node.Node, "adminUsername": "core", "customData": "[parameters('customData')]", "linuxConfiguration": map[string]any{"disablePasswordAuthentication": true, "ssh": map[string]any{"publicKeys": []any{map[string]any{"path": "/home/core/.ssh/authorized_keys", "keyData": node.PublicKey}}}}},
			"networkProfile":     map[string]any{"networkInterfaces": []any{map[string]any{"id": resourceID("networkInterfaces", nic), "properties": map[string]any{"primary": true, "deleteOption": "Delete"}}}},
			"storageProfile":     map[string]any{"imageReference": map[string]any{"publisher": "kinvolk", "offer": "flatcar-container-linux-corevm-amd64", "sku": "stable-gen2", "version": FlatcarVersion}, "osDisk": map[string]any{"name": node.Node + "-os", "createOption": "FromImage", "diskSizeGB": 32, "deleteOption": "Detach", "managedDisk": map[string]any{"storageAccountType": "StandardSSD_LRS"}}},
			"diagnosticsProfile": map[string]any{"bootDiagnostics": map[string]any{"enabled": false}},
		}},
	}
	return map[string]any{"$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#", "contentVersion": "1.0.0.0", "parameters": map[string]any{"customData": map[string]any{"type": "secureString"}}, "resources": resources}
}

func createAzureNode(ctx context.Context, o ClusterOptions, dir, name string, out io.Writer) error {
	runner := executil.OSRunner{}
	account := runner.Run(ctx, "az", "account", "show", "--query", "id", "-o", "tsv")
	if err := account.Err("read Azure subscription"); err != nil {
		return err
	}
	var subscription string
	// tsv contains only the subscription UUID.
	for _, ch := range account.Stdout {
		if ch != '\n' && ch != '\r' {
			subscription += string(ch)
		}
	}
	publicKey, err := disposableAzurePublicKey()
	if err != nil {
		return err
	}
	node := azureNode{PublicKey: publicKey, Subscription: subscription, Group: o.Name + "-rg", Node: name, Location: o.AzureLocation, Ownership: o.Name}
	exists := runner.Run(ctx, "az", "group", "exists", "--subscription", subscription, "--name", node.Group, "-o", "tsv")
	if err := exists.Err("check Azure group ownership"); err != nil {
		return err
	}
	if exists.Stdout != "false\n" && exists.Stdout != "false\r\n" {
		return fmt.Errorf("refusing existing Azure resource group %s", node.Group)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "output", name+".ign"))
	if err != nil {
		return err
	}
	if len(raw) > 65535 {
		return fmt.Errorf("Azure Ignition exceeds customData limit")
	}
	template := filepath.Join(dir, "azure-template.json")
	parameters := filepath.Join(dir, "azure-parameters.json")
	if err = fsutil.WriteJSON(template, azureTemplate(node, o.AzureVMSize)); err != nil {
		return err
	}
	if err = fsutil.WriteJSON(parameters, map[string]any{"customData": map[string]string{"value": base64.StdEncoding.EncodeToString(raw)}}); err != nil {
		return err
	}
	if err = fsutil.WriteJSON(filepath.Join(dir, "azure-node.json"), node); err != nil {
		return err
	}
	fmt.Fprintln(out, "Creating Azure VM:", name, "in", node.Location, "(no inbound access or artifact storage)")
	group := runner.Run(ctx, "az", "group", "create", "--subscription", subscription, "--name", node.Group, "--location", node.Location, "--tags", "infestation-mixed-id="+node.Ownership, "--query", "properties.provisioningState", "-o", "tsv")
	if err = group.Err("create mixed-test resource group"); err != nil {
		return err
	}
	return deployAzureNode(ctx, node, dir, out)
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

func deployAzureNode(ctx context.Context, node azureNode, dir string, out io.Writer) error {
	runner := executil.OSRunner{}
	subscription := node.Subscription
	template := filepath.Join(dir, "azure-template.json")
	parameters := filepath.Join(dir, "azure-parameters.json")
	deployed := runner.Run(ctx, "az", "deployment", "group", "create", "--subscription", subscription, "--resource-group", node.Group, "--name", "mixed", "--template-file", template, "--parameters", "@"+parameters, "--query", "properties.provisioningState", "-o", "tsv")
	if err := deployed.Err("deploy mixed-test Azure VM"); err != nil {
		return fmt.Errorf("%w; owned group %s retained for diagnosis", err, node.Group)
	}
	fmt.Fprintln(out, "Azure VM provisioned:", node.Node)
	resources := runner.Run(ctx, "az", "resource", "list", "--subscription", subscription, "--resource-group", node.Group, "--query", "[].{name:name,type:type}", "-o", "json")
	if err := resources.Err("record Azure resources"); err != nil {
		return err
	}
	var evidence any
	if err := json.Unmarshal([]byte(resources.Stdout), &evidence); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "azure-resources.json"), evidence)
}

func azureAction(ctx context.Context, node azureNode, action string, out io.Writer) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("unknown Azure VM action")
	}
	actual := "start"
	if action == "stop" {
		actual = "deallocate"
	}
	result := executil.OSRunner{}.Run(ctx, "az", "vm", actual, "--subscription", node.Subscription, "--resource-group", node.Group, "--name", node.Node)
	if err := result.Err("Azure VM " + actual); err != nil {
		return err
	}
	fmt.Fprintln(out, node.Node, "Azure", actual, "completed; disk preserved")
	return nil
}

// AzureAction affects only Azure compute, never the local VMs sharing this state.
func AzureAction(ctx context.Context, o Options, action string, out io.Writer) error {
	dir, _, err := loadCluster(o)
	if err != nil {
		return err
	}
	node, ok := loadAzureNode(dir)
	if !ok {
		return fmt.Errorf("no owned Azure node in this state")
	}
	if action == "status" {
		result := executil.OSRunner{Output: out}.Run(ctx, "az", "vm", "get-instance-view", "--subscription", node.Subscription, "--resource-group", node.Group, "--name", node.Node, "--query", "instanceView.statuses", "-o", "json")
		return result.Err("read Azure VM status")
	}
	return azureAction(ctx, node, action, out)
}
