package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestCustodyOfferMarshalRoundTrip(t *testing.T) {
	var custodyID, sourceHash [16]byte
	rand.Read(custodyID[:])
	rand.Read(sourceHash[:])

	offer := &CustodyOffer{
		CustodyID:  custodyID,
		SourceHash: sourceHash,
		DeliveryID: 42,
		Payload:    []byte("test custody payload"),
	}

	data := MarshalCustodyOffer(offer)
	if data[0] != CustodyOfferType {
		t.Fatalf("expected type 0x%02x, got 0x%02x", CustodyOfferType, data[0])
	}

	parsed, err := UnmarshalCustodyOffer(data)
	if err != nil {
		t.Fatalf("UnmarshalCustodyOffer: %v", err)
	}

	if parsed.CustodyID != custodyID {
		t.Fatal("custody ID mismatch")
	}
	if parsed.SourceHash != sourceHash {
		t.Fatal("source hash mismatch")
	}
	if parsed.DeliveryID != 42 {
		t.Fatalf("delivery ID: got %d, want 42", parsed.DeliveryID)
	}
	if !bytes.Equal(parsed.Payload, offer.Payload) {
		t.Fatal("payload mismatch")
	}
}

func TestCustodyACKMarshalRoundTrip(t *testing.T) {
	var custodyID, acceptorHash [16]byte
	rand.Read(custodyID[:])
	rand.Read(acceptorHash[:])

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	ack := SignCustodyACK(custodyID, acceptorHash, priv)

	data := MarshalCustodyACK(ack)
	if data[0] != CustodyACKType {
		t.Fatalf("expected type 0x%02x, got 0x%02x", CustodyACKType, data[0])
	}

	parsed, err := UnmarshalCustodyACK(data)
	if err != nil {
		t.Fatalf("UnmarshalCustodyACK: %v", err)
	}

	if parsed.CustodyID != custodyID {
		t.Fatal("custody ID mismatch")
	}
	if parsed.AcceptorHash != acceptorHash {
		t.Fatal("acceptor hash mismatch")
	}

	if !VerifyCustodyACK(parsed, pub) {
		t.Fatal("signature verification failed")
	}
}

func TestCustodyACKBadSignature(t *testing.T) {
	var custodyID, acceptorHash [16]byte
	rand.Read(custodyID[:])
	rand.Read(acceptorHash[:])

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	ack := SignCustodyACK(custodyID, acceptorHash, priv)
	if VerifyCustodyACK(ack, otherPub) {
		t.Fatal("signature should not verify with wrong key")
	}
}

func TestNewCustodyID(t *testing.T) {
	id1, err := NewCustodyID()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := NewCustodyID()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("two UUIDs should not be identical")
	}
	// Check UUID v4 bits.
	if id1[6]>>4 != 4 {
		t.Fatal("UUID version should be 4")
	}
	if id1[8]>>6 != 2 {
		t.Fatal("UUID variant should be 2")
	}
}

func TestIsCustodyOfferACK(t *testing.T) {
	offer := &CustodyOffer{Payload: []byte("test")}
	data := MarshalCustodyOffer(offer)
	if !IsCustodyOffer(data) {
		t.Fatal("should detect custody offer")
	}
	if IsCustodyACK(data) {
		t.Fatal("should not detect custody ACK from offer data")
	}
}

func TestCustodyManagerRegisterAndACK(t *testing.T) {
	cm := NewCustodyManager(5 * time.Second)

	offer := &CustodyOffer{DeliveryID: 1, Payload: []byte("test")}
	rand.Read(offer.CustodyID[:])

	ch := cm.RegisterOffer(offer)
	if cm.PendingCount() != 1 {
		t.Fatalf("expected 1 pending, got %d", cm.PendingCount())
	}

	ack := &CustodyACK{CustodyID: offer.CustodyID}
	if !cm.HandleACK(ack) {
		t.Fatal("HandleACK should return true for matching offer")
	}

	select {
	case received := <-ch:
		if received.CustodyID != ack.CustodyID {
			t.Fatal("received ACK has wrong custody ID")
		}
	default:
		t.Fatal("expected ACK on channel")
	}
}

func TestCustodyManagerUnknownACK(t *testing.T) {
	cm := NewCustodyManager(5 * time.Second)

	ack := &CustodyACK{}
	rand.Read(ack.CustodyID[:])
	if cm.HandleACK(ack) {
		t.Fatal("HandleACK should return false for unknown custody ID")
	}
}

func TestCustodyStateString(t *testing.T) {
	if CustodyOffered.String() != "offered" {
		t.Fatal("offered string mismatch")
	}
	if CustodyAccepted.String() != "accepted" {
		t.Fatal("accepted string mismatch")
	}
	if CustodyExpired.String() != "expired" {
		t.Fatal("expired string mismatch")
	}
}
