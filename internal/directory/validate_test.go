package directory

import (
	"errors"
	"testing"
)

// TestCanonicalMatchesTheBridge: the forms the Bridge's own parsers accept
// (internal/tetra ParseAddress, internal/gateway ParseCellAddress) and the
// stored form of each.
func TestCanonicalMatchesTheBridge(t *testing.T) {
	ok := []struct {
		kind     AddressKind
		in, want string
	}{
		{KindTETRA, "2000", "tetra:2000"},
		{KindTETRA, "tetra:2000", "tetra:2000"},
		{KindTETRA, "00002000", "tetra:2000"},
		{KindTETRA, "1", "tetra:1"},
		{KindTETRA, "16777214", "tetra:16777214"},
		{KindTETRA, "7530236100002000", "tetra:7530236100002000"},
		{KindCell, "1001", "cell:1001"},
		{KindCell, "cell:1001", "cell:1001"},
		{KindCell, "CELL:1001", "cell:1001"},
		{KindCell, "+31612345678", "cell:+31612345678"},
		{KindMQTT, "bridge:abc-123", "bridge:abc-123"},
		{KindMQTT, "device:300434063000000", "device:300434063000000"},
		{KindSMS, " +31612345678 ", "+31612345678"},
		{KindMeshtastic, "!abcd1234", "!abcd1234"},
	}
	for _, c := range ok {
		got, err := c.kind.Canonical(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s %q: %q %v, want %q", c.kind, c.in, got, err, c.want)
		}
	}
	bad := []struct {
		kind AddressKind
		in   string
	}{
		{KindTETRA, ""}, {KindTETRA, "0"}, {KindTETRA, "16777215"}, {KindTETRA, "12a"}, {KindTETRA, "tetra:"},
		{KindTETRA, "123456789012345678"}, {KindTETRA, "-5"},
		{KindCell, "cell:"}, {KindCell, "+"}, {KindCell, "1+2"}, {KindCell, "123456789012345678901"}, {KindCell, "abc"},
		{KindMQTT, "abc"}, {KindMQTT, "bridge:"}, {KindMQTT, "device:a+b"}, {KindMQTT, "bridge:a#"}, {KindMQTT, "bridge:a b"},
		{KindSMS, ""},
	}
	for _, c := range bad {
		if got, err := c.kind.Canonical(c.in); !errors.Is(err, ErrInvalidAddressValue) {
			t.Errorf("%s %q: %q %v, want ErrInvalidAddressValue", c.kind, c.in, got, err)
		}
	}
}
