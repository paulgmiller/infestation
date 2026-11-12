package ignition

import (
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
	"infestation/internal/nodebootstrap"
	"strings"
)

func membershipProfile(doc *types.Config, c cluster.Config, n cluster.Node, b Bootstrap) {
	if n.Init {
		doc.Storage.Files = append(doc.Storage.Files, File(nodebootstrap.JoinBundlePath, b.JoinBundle, 0600))
	}
	if b.ScaleSet != nil {
		identity := `[Unit]
Description=Derive node identity from Azure VMSS instance metadata
Requires=infestation-helper-build.service
After=infestation-helper-build.service network-online.target
Before=infestation-tailnet.service
StartLimitIntervalSec=0
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/infestation/infestation-node identity
TimeoutStartSec=120s
Restart=on-failure
RestartSec=15s
[Install]
WantedBy=multi-user.target
`
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-instance-identity.service", Enabled: ptr(true), Contents: &identity})
		for i := range doc.Systemd.Units {
			if doc.Systemd.Units[i].Name == "infestation-tailnet.service" {
				dependency := "[Unit]\nRequires=infestation-instance-identity.service\nAfter=infestation-instance-identity.service\n"
				doc.Systemd.Units[i].Dropins = append(doc.Systemd.Units[i].Dropins, types.Dropin{Name: "15-instance-identity.conf", Contents: &dependency})
			}
		}
	}
	join := `[Unit]
Description=Discover cluster and install leaf credentials over the tailnet
Requires=infestation-tailnet.service
After=infestation-tailnet.service
Before=cockroachdb.service k3s.service k3s-agent.service
StartLimitIntervalSec=0
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/infestation/infestation-node enroll
TimeoutStartSec=0
Restart=on-failure
RestartSec=15s
[Install]
WantedBy=multi-user.target
`
	serve := `[Unit]
Description=Share cluster enrollment material with tailnet administrators
Requires=infestation-join.service
After=infestation-join.service
StartLimitIntervalSec=0
[Service]
Type=simple
ExecStart=/opt/infestation/infestation-node membership
Restart=always
RestartSec=15s
[Install]
WantedBy=multi-user.target
`
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-join.service", Enabled: ptr(true), Contents: &join}, types.Unit{Name: "infestation-membership.service", Enabled: ptr(true), Contents: &serve})
	for i := range doc.Systemd.Units {
		u := &doc.Systemd.Units[i]
		if u.Name == "k3s.service" || u.Name == "k3s-agent.service" {
			dependency := "[Unit]\nRequires=infestation-join.service\nAfter=infestation-join.service\n"
			u.Dropins = append(u.Dropins, types.Dropin{Name: "15-enrollment.conf", Contents: &dependency})
		}
		if u.Name == "infestation-bootstrap.service" && u.Contents != nil {
			value := strings.Replace(*u.Contents, "[Unit]\n", "[Unit]\nWants=infestation-membership.service\nAfter=infestation-membership.service\n", 1)
			u.Contents = &value
		}
	}
}
