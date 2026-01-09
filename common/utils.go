package common

import (
	"strconv"
	"strings"

	"github.com/blockchain-data-standards/manifesto/evm"
)

const KeySeparator = "|"

type ContextKey string

func HexToUint64(hexValue string) (uint64, error) {
	s := strings.TrimSpace(hexValue)
	if s != "" && !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return u, nil
		}
	}
	return evm.HexToUint64(s)
}

func HexToInt64(hexValue string) (int64, error) {
	s := strings.TrimSpace(hexValue)
	if s != "" && !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		// Some upstreams incorrectly return EVM quantities as base-10 strings (e.g. "44007042")
		// instead of 0x-prefixed hex. Be tolerant to avoid breaking upstream health tracking.
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
	}
	return evm.HexToInt64(s)
}

func HexToBytes(hexValue string) ([]byte, error) {
	return evm.HexToBytes(hexValue)
}

func NormalizeHex(value interface{}) (string, error) {
	return evm.NormalizeHex(value)
}

func RemoveDuplicates(slice []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range slice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

// IsSemiValidJson checks the first byte to see if "potentially" valid json is present.
// This is not a full json.Valid check, but it is good enough for high speed detection of wrong HTML responses
// from upstreams.
func IsSemiValidJson(data []byte) bool {
	return len(data) > 0 && (data[0] == '{' || data[0] == '[' || data[0] == '"' || data[0] == 'n' || data[0] == 't' || data[0] == 'f')
}
