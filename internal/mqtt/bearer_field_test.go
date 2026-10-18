package mqtt

import (
	"encoding/json"
	"testing"
)

// The channel field of mo/decoded is a bearer name, or a Bridge's mesh
// channel index (a number), or absent. [MESHSAT-1584]
func TestBearerFieldTakesANameANumberOrNull(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`{"channel":"sms"}`, "sms", true},
		{`{"channel":"iridium_imt"}`, "iridium_imt", true},
		{`{"channel":3}`, "", true},
		{`{"channel":0}`, "", true},
		{`{"channel":null}`, "", true},
		{`{}`, "", true},
		{`{"channel":{"x":1}}`, "", false},
		{`{"channel":true}`, "", false},
	} {
		var m struct {
			Channel BearerField `json:"channel"`
		}
		err := json.Unmarshal([]byte(tc.in), &m)
		if (err == nil) != tc.ok || string(m.Channel) != tc.want {
			t.Errorf("%s: %q %v; want %q ok=%t", tc.in, m.Channel, err, tc.want, tc.ok)
		}
	}
}
