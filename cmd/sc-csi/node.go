package main

import (
	"log/slog"
	"path/filepath"

	"github.com/charlie-horse/sc-csi/internal/driver"
	"github.com/charlie-horse/sc-csi/internal/fc"
	"github.com/charlie-horse/sc-csi/internal/mounter"
)

// nodeDriver builds the node-side Driver on the real FC and mount back ends.
// A non-empty hostRoot (e.g. /host) makes the multipath tools run chrooted
// into the host root; hostPath lists directories put in front of PATH.
func nodeDriver(log *slog.Logger, nodeName, hostRoot, hostPath string) (*driver.Driver, error) {
	var extra []string
	if hostPath != "" {
		extra = filepath.SplitList(hostPath)
	}
	conn := fc.New(fc.Options{Chroot: hostRoot, ExtraPath: extra, Log: log})
	wwpns, err := conn.WWPNs()
	if err != nil {
		return nil, err
	}
	log.Info("FC initiators", "wwpns", wwpns)
	return &driver.Driver{
		Cfg:   driver.Config{NodeName: nodeName, Version: version},
		Log:   log,
		Conn:  conn,
		Mount: mounter.New(),
		WWPNs: conn.WWPNs,
	}, nil
}
