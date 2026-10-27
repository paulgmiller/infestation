package ignition

import (
	"encoding/json"
	"fmt"
)

// ValidateNoSSH rejects stale profiles that still install login keys or enable sshd.
func ValidateNoSSH(data []byte) error {
	var doc struct {
		Passwd struct {
			Users []struct {
				Keys []string `json:"sshAuthorizedKeys"`
			} `json:"users"`
		} `json:"passwd"`
		Systemd struct {
			Units []struct {
				Name string `json:"name"`
				Mask bool   `json:"mask"`
			} `json:"units"`
		} `json:"systemd"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	for _, user := range doc.Passwd.Users {
		if len(user.Keys) != 0 {
			return fmt.Errorf("SSH authorized keys are no longer supported")
		}
	}
	masked := map[string]bool{}
	for _, unit := range doc.Systemd.Units {
		masked[unit.Name] = unit.Mask
	}
	for _, name := range []string{"sshd.service", "sshd.socket", "ssh.service", "ssh.socket", "sshd@.service"} {
		if !masked[name] {
			return fmt.Errorf("SSH unit %s must be masked", name)
		}
	}
	return nil
}
