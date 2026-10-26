package tak

import "time"

// GeoChat in the form ATAK writes it (MESHSAT-1706): the Hub's operators write
// to a crew this way, and a crew's answer comes back this way. The Bridge and
// the apps build the same shape (meshsat internal/gateway/tak_chat.go).

// OperatorUIDPrefix starts the uid a Hub operator writes to a crew under, and
// so the uid a crew's answer to that operator is addressed to (MESHSAT-1706).
// It lies under the Hub's own "meshsat-hub-" prefix, which no client may use.
const OperatorUIDPrefix = "meshsat-hub-op-"

// ChatRoom is ATAK's name and id for the room every contact is in.
const ChatRoom = "All Chat Rooms"

// chatStale is a GeoChat's lifetime, ATAK's own: a day.
const chatStale = 24 * time.Hour

// ChatPeer is one end of a chat: a TAK uid and its callsign.
type ChatPeer struct {
	UID      string
	Callsign string
}

// BuildGeoChat writes a chat from one peer to another: __chat and chatgrp name
// both ends, link the sender, remarks the words and the recipient, and a dest
// asks a server to deliver it to the recipient only.
func BuildGeoChat(from, to ChatPeer, text, messageID string, now time.Time) CotEvent {
	now = now.UTC()
	ts := now.Format(cotTimeFormat)
	return CotEvent{
		Version: "2.0",
		UID:     "GeoChat." + from.UID + "." + to.UID + "." + messageID,
		Type:    TypeChat,
		How:     "h-g-i-g-o",
		Time:    ts,
		Start:   ts,
		Stale:   now.Add(chatStale).Format(cotTimeFormat),
		Point:   CotPoint{Hae: 9999999, Ce: 9999999, Le: 9999999},
		Detail: &CotDetail{
			Chat: &CotChat{
				Parent:         "RootContactGroup",
				GroupOwner:     "false",
				MessageID:      messageID,
				Chatroom:       to.Callsign,
				ID:             to.UID,
				SenderCallsign: from.Callsign,
				Group:          &CotChatGroup{UID0: from.UID, UID1: to.UID, ID: to.UID},
			},
			Link:    &CotLink{UID: from.UID, Type: TypePosition, Relation: "p-p"},
			Remarks: &CotRemarks{Source: "BAO.F.ATAK." + from.UID, To: to.UID, Time: ts, Text: text},
			Marti:   &CotMarti{Dests: []CotDest{{UID: to.UID, Callsign: to.Callsign}}},
		},
	}
}
