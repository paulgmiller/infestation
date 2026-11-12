package ignition

import (
	"fmt"
	"strings"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
	"infestation/internal/nodebootstrap"
)

// Upholds revives dependent units whose original start jobs failed while a
// prerequisite was unavailable. Requires/After alone do not retry those jobs.
// Only persistent services/timers are upheld, never completing oneshots.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#Upholds=
func recoveryProfile(doc *types.Config, n cluster.Node) {
	service := "k3s.service"
	if n.Worker() {
		service = "k3s-agent.service"
	}
	next := map[string][]string{
		"infestation-helper-build.service":      {"infestation-tailnet.service"},
		"infestation-instance-identity.service": {"infestation-tailnet.service"},
		"infestation-tailnet.service":           {"infestation-join.service"},
		"infestation-join.service":              {service, "infestation-membership.service", "infestation-bootstrap.timer"},
	}
	for _, u := range doc.Systemd.Units {
		if u.Name == "infestation-instance-identity.service" {
			next["infestation-helper-build.service"] = []string{u.Name}
		}
	}
	if n.Database() {
		next["infestation-join.service"] = append(next["infestation-join.service"], "cockroachdb.service")
	}
	if !n.Worker() {
		next["infestation-join.service"] = append(next["infestation-join.service"], "infestation-access.timer")
	}
	for i := range doc.Systemd.Units {
		u := &doc.Systemd.Units[i]
		if targets := next[u.Name]; len(targets) > 0 {
			value := "[Unit]\nUpholds=" + strings.Join(targets, " ") + "\n"
			u.Dropins = append(u.Dropins, types.Dropin{Name: "30-recovery.conf", Contents: &value})
		}
	}
	check := fmt.Sprintf(`#!/bin/sh
set -eu
if ! version=$(/opt/infestation/infestation-node --contract-version 2>/dev/null) || test "$version" != '%s'; then
  echo 'incompatible node helper: generated services require helper contract %s; select a compatible published helper source revision' >&2
  exit 1
fi
`, nodebootstrap.HelperContractVersion, nodebootstrap.HelperContractVersion)
	doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/check-helper", []byte(check), 0755))
	if n.Database() {
		check := `#!/bin/sh
set -eu
: "${INFESTATION_SEEDS:?missing INFESTATION_SEEDS; enrollment did not prepare the database environment}"
: "${INFESTATION_DATABASE_ADDRESS:?missing INFESTATION_DATABASE_ADDRESS; check helper compatibility and rerun enrollment}"
`
		if n.Locality != "" {
			check += ": \"${INFESTATION_LOCALITY:?missing INFESTATION_LOCALITY; check helper compatibility and rerun enrollment}\"\n"
		}
		doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/check-database-environment", []byte(check), 0755))
	}
	for i := range doc.Systemd.Units {
		if doc.Systemd.Units[i].Name == "infestation-join.service" {
			value := "[Service]\nExecStartPre=/opt/infestation/check-helper\n"
			doc.Systemd.Units[i].Dropins = append(doc.Systemd.Units[i].Dropins, types.Dropin{Name: "20-helper-contract.conf", Contents: &value})
		}
	}
}
