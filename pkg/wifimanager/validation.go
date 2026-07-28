package wifimanager

import "fmt"

// ValidateWiFiPassword checks a WPA/WPA2 passphrase: 8..63 printable ASCII
// characters. Callers skip this for open networks (empty password).
func ValidateWiFiPassword(password string) error {
	length := len(password)
	if length < 8 {
		return fmt.Errorf("Wi-Fi password must be at least 8 characters (WPA/WPA2 requirement)")
	}
	if length > 63 {
		return fmt.Errorf("Wi-Fi password cannot exceed 63 characters (WPA/WPA2 limitation)")
	}
	// Range over bytes, not runes: the WPA passphrase is an ASCII byte string,
	// and a multi-byte rune would silently pass a per-rune bound check while
	// still being rejected by the supplicant.
	for i := 0; i < length; i++ {
		if password[i] < 32 || password[i] > 126 {
			return fmt.Errorf("Wi-Fi password contains a non-printable character at position %d (ASCII printable only)", i)
		}
	}
	return nil
}
