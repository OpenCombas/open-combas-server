package server

import (
	"ChromehoundsStatusServer/constants"
	"testing"
)

// TestBattleReportAck verifies the mission-result ack: the parser sub_823BD940 reads the status at
// body[0] and conquest-event flags at body[2]/[4]/[6]. The static server signals "Normal End" with no
// special events ("1,0,0,0").
func TestBattleReportAck(t *testing.T) {
	state := CreateBattleReportAck(squadXuid, [8]byte{})
	buf := encodeToBuffer(state, constants.BattleReportResponseSize, t)

	if len(buf) != 39 {
		t.Fatalf("response size = %d, want 39", len(buf))
	}

	body := buf[constants.MinHelloMessageSize:]
	if body[0] != '1' {
		t.Errorf("status (body[0]) = %q, want '1' (Normal End)", body[0])
	}
	for _, off := range []int{2, 4, 6} {
		if body[off] != '0' {
			t.Errorf("event flag body[%d] = %q, want '0' (no event)", off, body[off])
		}
	}
	for _, off := range []int{1, 3, 5} {
		if body[off] != ',' {
			t.Errorf("separator body[%d] = %q, want ','", off, body[off])
		}
	}
}
