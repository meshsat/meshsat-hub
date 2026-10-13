package lxmfhub

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/msgpack"
)

// maxFieldsJSON bounds what one message's fields may put in the inbox row;
// an attachment that large belongs to a later "files" feature, not a text
// column.
const maxFieldsJSON = 64 * 1024

// fieldsJSON renders a message's LXMF fields as JSON for the inbox row:
// integer keys as decimal strings, binary values base64, "" for no fields,
// and a marker object when the result would be too large.
func fieldsJSON(v lxmf.Value) string {
	if v.Kind != msgpack.KindMap || len(v.Map) == 0 {
		return ""
	}
	b, err := json.Marshal(toJSON(v))
	if err != nil {
		return ""
	}
	if len(b) > maxFieldsJSON {
		return `{"_truncated":true,"_bytes":` + strconv.Itoa(len(b)) + `}`
	}
	return string(b)
}

func toJSON(v lxmf.Value) any {
	switch v.Kind {
	case msgpack.KindNil:
		return nil
	case msgpack.KindBool:
		return v.Bool
	case msgpack.KindInt:
		return v.Int
	case msgpack.KindUint:
		return v.Uint
	case msgpack.KindFloat:
		return v.Float
	case msgpack.KindStr:
		return v.Str
	case msgpack.KindBin:
		return base64.StdEncoding.EncodeToString(v.Bin)
	case msgpack.KindArray:
		out := make([]any, 0, len(v.Array))
		for _, e := range v.Array {
			out = append(out, toJSON(e))
		}
		return out
	case msgpack.KindMap:
		out := make(map[string]any, len(v.Map))
		for _, kv := range v.Map {
			out[keyString(kv.Key)] = toJSON(kv.Val)
		}
		return out
	default:
		return base64.StdEncoding.EncodeToString(v.Ext)
	}
}

func keyString(k lxmf.Value) string {
	switch k.Kind {
	case msgpack.KindStr:
		return k.Str
	case msgpack.KindInt:
		return strconv.FormatInt(k.Int, 10)
	case msgpack.KindUint:
		return strconv.FormatUint(k.Uint, 10)
	case msgpack.KindBin:
		return base64.StdEncoding.EncodeToString(k.Bin)
	default:
		return "?"
	}
}
