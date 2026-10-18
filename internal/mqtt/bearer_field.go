package mqtt

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BearerField is the "channel" field of mo/decoded. Most publishers write a
// bearer name ("sms", "iridium", "iridium_imt", "mesh"); a MeshSat Bridge
// writes its mesh channel index there, a number, and leaves it out for
// channel 0. A number names no bearer, so it reads as none, as channel 0
// always did. Decoded as a plain string it failed the whole message, and
// the routing engine and the message store dropped every Bridge mesh text
// on a channel other than 0. [MESHSAT-1584]
type BearerField string

// UnmarshalJSON takes a string, a number or null.
func (b *BearerField) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) > 0 && data[0] == '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*b = BearerField(s)
	case bytes.Equal(data, []byte("null")):
		*b = ""
	default:
		var n json.Number
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("channel is neither a bearer name nor a number: %s", data)
		}
		*b = ""
	}
	return nil
}
