package wifimanager

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"github.com/mdlayher/wifi"
	"golang.org/x/sys/unix"
)

const (
	informationElementSSID = 0
	informationElementRSN  = 48
	informationElementWPA  = 221
	capabilityPrivacy      = 1 << 4

	scanSSIDWildcardAttribute = 1
)

func triggerInterfaceScan(ctx context.Context, _ scanClient, intf *wifi.Interface) error {
	return triggerScanNL80211(ctx, intf)
}

func triggerScanNL80211(ctx context.Context, intf *wifi.Interface) error {
	if intf == nil {
		return fmt.Errorf("nil WiFi interface")
	}

	conn, err := genetlink.Dial(&netlink.Config{Strict: true})
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	family, err := conn.GetFamily(unix.NL80211_GENL_NAME)
	if err != nil {
		return err
	}

	scanGroupID := uint32(0)
	for _, group := range family.Groups {
		if group.Name == unix.NL80211_MULTICAST_GROUP_SCAN {
			scanGroupID = group.ID
			break
		}
	}

	if scanGroupID != 0 {
		if err := conn.JoinGroup(scanGroupID); err != nil {
			return err
		}
		defer func() { _ = conn.LeaveGroup(scanGroupID) }()
	}

	data, err := encodeTriggerScanData(intf)
	if err != nil {
		return err
	}

	_, err = conn.Send(
		genetlink.Message{
			Header: genetlink.Header{
				Command: unix.NL80211_CMD_TRIGGER_SCAN,
				Version: family.Version,
			},
			Data: data,
		},
		family.ID,
		netlink.Request,
	)
	if err != nil {
		return err
	}

	if scanGroupID == 0 {
		return nil
	}

	return waitForScanDoneNL80211(ctx, conn, intf.Index, family.Version)
}

func encodeTriggerScanData(intf *wifi.Interface) ([]byte, error) {
	if intf == nil {
		return nil, fmt.Errorf("nil WiFi interface")
	}

	ae := netlink.NewAttributeEncoder()
	ae.Uint32(unix.NL80211_ATTR_IFINDEX, uint32(intf.Index))
	ae.Nested(unix.NL80211_ATTR_SCAN_SSIDS, func(ae *netlink.AttributeEncoder) error {
		// nl80211 requires a zero-length SSID attribute for wildcard active scans.
		ae.Bytes(scanSSIDWildcardAttribute, nil)
		return nil
	})
	return ae.Encode()
}

func waitForScanDoneNL80211(ctx context.Context, conn *genetlink.Conn, intfIndex int, familyVersion uint8) error {
	for ctx.Err() == nil {
		msgs, _, err := conn.Receive()
		if err != nil {
			return err
		}

		for _, msg := range msgs {
			if msg.Header.Version != familyVersion {
				continue
			}

			switch msg.Header.Command {
			case unix.NL80211_CMD_SCAN_ABORTED:
				return fmt.Errorf("scan aborted by kernel")
			case unix.NL80211_CMD_NEW_SCAN_RESULTS:
				attrs, err := netlink.UnmarshalAttributes(msg.Data)
				if err != nil {
					return fmt.Errorf("scan completion validation failed: %w", err)
				}

				eventIndex, ok := nl80211InterfaceIndex(attrs)
				if !ok || eventIndex == intfIndex {
					return nil
				}
			}
		}
	}

	return ctx.Err()
}

func nl80211InterfaceIndex(attrs []netlink.Attribute) (int, bool) {
	for _, attr := range attrs {
		if attr.Type == unix.NL80211_ATTR_IFINDEX {
			return int(nlenc.Uint32(attr.Data)), true
		}
	}
	return 0, false
}

func readAccessPointsNL80211(intf *wifi.Interface) ([]*wifi.BSS, error) {
	if intf == nil {
		return nil, fmt.Errorf("nil WiFi interface")
	}

	conn, err := genetlink.Dial(&netlink.Config{Strict: true})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	family, err := conn.GetFamily(unix.NL80211_GENL_NAME)
	if err != nil {
		return nil, err
	}

	ae := netlink.NewAttributeEncoder()
	ae.Uint32(unix.NL80211_ATTR_IFINDEX, uint32(intf.Index))

	data, err := ae.Encode()
	if err != nil {
		return nil, err
	}

	msgs, err := conn.Execute(
		genetlink.Message{
			Header: genetlink.Header{
				Command: unix.NL80211_CMD_GET_SCAN,
				Version: family.Version,
			},
			Data: data,
		},
		family.ID,
		netlink.Request|netlink.Dump,
	)
	if err != nil {
		return nil, err
	}

	return parseNL80211ScanMessages(msgs)
}

func parseNL80211ScanMessages(msgs []genetlink.Message) ([]*wifi.BSS, error) {
	accessPoints := make([]*wifi.BSS, 0, len(msgs))
	for _, msg := range msgs {
		attrs, err := netlink.UnmarshalAttributes(msg.Data)
		if err != nil {
			return nil, err
		}

		for _, attr := range attrs {
			if attr.Type != unix.NL80211_ATTR_BSS {
				continue
			}

			bss, err := parseNL80211BSS(attr.Data)
			if err != nil {
				return nil, err
			}
			accessPoints = append(accessPoints, bss)
		}
	}

	return accessPoints, nil
}

func parseNL80211BSS(data []byte) (*wifi.BSS, error) {
	attrs, err := netlink.UnmarshalAttributes(data)
	if err != nil {
		return nil, err
	}

	var (
		bss          wifi.BSS
		capabilities uint16
		ies          []byte
		beaconIEs    []byte
	)

	for _, attr := range attrs {
		switch attr.Type {
		case unix.NL80211_BSS_BSSID:
			bss.BSSID = net.HardwareAddr(append([]byte(nil), attr.Data...))
		case unix.NL80211_BSS_FREQUENCY:
			bss.Frequency = int(nlenc.Uint32(attr.Data))
		case unix.NL80211_BSS_BEACON_INTERVAL:
			bss.BeaconInterval = time.Duration(nlenc.Uint16(attr.Data)) * 1024 * time.Microsecond
		case unix.NL80211_BSS_SEEN_MS_AGO:
			bss.LastSeen = time.Duration(nlenc.Uint32(attr.Data)) * time.Millisecond
		case unix.NL80211_BSS_STATUS:
			bss.Status = wifi.BSSStatus(nlenc.Uint32(attr.Data))
		case unix.NL80211_BSS_SIGNAL_MBM:
			bss.Signal = nlenc.Int32(attr.Data)
		case unix.NL80211_BSS_SIGNAL_UNSPEC:
			bss.SignalUnspecified = nlenc.Uint32(attr.Data)
		case unix.NL80211_BSS_CAPABILITY:
			capabilities = nlenc.Uint16(attr.Data)
		case unix.NL80211_BSS_INFORMATION_ELEMENTS:
			ies = attr.Data
		case unix.NL80211_BSS_BEACON_IES:
			beaconIEs = attr.Data
		}
	}

	ssid, encrypted := parseScanInformationElements(ies)
	if ssid == "" && len(beaconIEs) > 0 {
		ssid, encrypted = parseScanInformationElements(beaconIEs)
	} else if _, beaconEncrypted := parseScanInformationElements(beaconIEs); beaconEncrypted {
		encrypted = true
	}

	bss.SSID = ssid
	if encrypted || capabilities&capabilityPrivacy != 0 {
		bss.RSN = wifi.RSNInfo{
			Version:         1,
			PairwiseCiphers: []wifi.RSNCipher{wifi.RSNCipherCCMP128},
		}
	}

	return &bss, nil
}

func parseScanInformationElements(ies []byte) (string, bool) {
	var (
		ssid      string
		encrypted bool
	)

	for len(ies) >= 2 {
		id := ies[0]
		length := int(ies[1])
		ies = ies[2:]
		if length > len(ies) {
			break
		}

		data := ies[:length]
		ies = ies[length:]

		switch id {
		case informationElementSSID:
			ssid = string(data)
		case informationElementRSN:
			encrypted = true
		case informationElementWPA:
			if isWPAInformationElement(data) {
				encrypted = true
			}
		}
	}

	return ssid, encrypted
}

func isWPAInformationElement(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == 0x00 &&
		data[1] == 0x50 &&
		data[2] == 0xf2 &&
		data[3] == 0x01
}
