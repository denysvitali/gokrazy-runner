//go:build !linux

package wifimanager

import (
	"context"
	"fmt"

	"github.com/mdlayher/wifi"
)

func triggerInterfaceScan(ctx context.Context, cl scanClient, intf *wifi.Interface) error {
	return cl.Scan(ctx, intf)
}

func readAccessPointsNL80211(_ *wifi.Interface) ([]*wifi.BSS, error) {
	return nil, fmt.Errorf("raw nl80211 scanning is only available on Linux")
}
