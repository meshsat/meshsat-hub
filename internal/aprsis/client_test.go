package aprsis

import (
	"strings"
	"testing"
)

func TestFormatPosition_NorthEast(t *testing.T) {
	pkt := FormatPosition("PA3XYZ", 10, 52.3676, 4.9041, "MeshSat via Iridium")

	if !strings.HasPrefix(pkt, "PA3XYZ-10>APMSHT,TCPIP*:!") {
		t.Errorf("bad prefix: %s", pkt)
	}
	if !strings.Contains(pkt, "N/") {
		t.Errorf("missing N hemisphere: %s", pkt)
	}
	if !strings.Contains(pkt, "E-") {
		t.Errorf("missing E hemisphere: %s", pkt)
	}
	if !strings.Contains(pkt, "MeshSat via Iridium") {
		t.Errorf("missing comment: %s", pkt)
	}
}

func TestFormatPosition_SouthWest(t *testing.T) {
	pkt := FormatPosition("LU1ABC", 10, -34.6037, -58.3816, "Buenos Aires")

	if !strings.Contains(pkt, "S/") {
		t.Errorf("missing S hemisphere: %s", pkt)
	}
	if !strings.Contains(pkt, "W-") {
		t.Errorf("missing W hemisphere: %s", pkt)
	}
}

func TestFormatPosition_LatLonPrecision(t *testing.T) {
	// 52°22.06'N / 004°54.25'E
	pkt := FormatPosition("PA3XYZ", 10, 52.0+22.06/60.0, 4.0+54.25/60.0, "test")

	// Should contain "5222.06N" and "00454.25E"
	if !strings.Contains(pkt, "5222.06N") {
		t.Errorf("expected 5222.06N in: %s", pkt)
	}
	if !strings.Contains(pkt, "00454.25E") {
		t.Errorf("expected 00454.25E in: %s", pkt)
	}
}

func TestFormatPosition_Tocall(t *testing.T) {
	pkt := FormatPosition("TEST", 10, 0, 0, "test")

	// APMSHT = MeshSat Hub tocall
	if !strings.Contains(pkt, "APMSHT") {
		t.Errorf("missing tocall APMSHT: %s", pkt)
	}
	// TCPIP* = internet-gated
	if !strings.Contains(pkt, "TCPIP*") {
		t.Errorf("missing TCPIP* path: %s", pkt)
	}
}

func TestFormatPosition_PositionIndicator(t *testing.T) {
	pkt := FormatPosition("TEST", 10, 52.0, 4.0, "test")

	// Should start position data with '!' (no timestamp)
	idx := strings.Index(pkt, ":!")
	if idx < 0 {
		t.Errorf("missing :! position indicator: %s", pkt)
	}
}
