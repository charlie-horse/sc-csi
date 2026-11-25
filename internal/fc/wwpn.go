package fc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WWPNs returns the node's FC initiator port names from sysRoot/class/fc_host
// as 16 lower case hex digits without 0x. It returns only ports whose
// port_state is "Online"; when none is online it returns all ports, so a node
// whose links are down still gets a usable node ID.
func WWPNs(sysRoot string) ([]string, error) {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	hosts, err := filepath.Glob(filepath.Join(sysRoot, "class", "fc_host", "host*"))
	if err != nil {
		return nil, err
	}
	var online, all []string
	seen := map[string]bool{}
	for _, h := range hosts {
		b, err := os.ReadFile(filepath.Join(h, "port_name"))
		if err != nil {
			continue
		}
		w := NormalizeWWPN(string(b))
		if !isHex(w, 16) || seen[w] {
			continue
		}
		seen[w] = true
		all = append(all, w)
		st, _ := os.ReadFile(filepath.Join(h, "port_state"))
		if strings.EqualFold(strings.TrimSpace(string(st)), "Online") {
			online = append(online, w)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no FC host ports under %s",
			filepath.Join(sysRoot, "class", "fc_host"))
	}
	out := online
	if len(out) == 0 {
		out = all
	}
	sort.Strings(out)
	return out, nil
}

// WWPNs returns the node's FC initiator ports, read from the Connector's SysRoot.
func (c *Connector) WWPNs() ([]string, error) {
	return WWPNs(c.opts.SysRoot)
}
