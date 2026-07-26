package netdiag

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"testing"
)

// rfc5769TxID is the transaction ID from the RFC 5769 sample messages; the
// hand-computed XOR-MAPPED-ADDRESS vectors below are derived from it.
var rfc5769TxID = [12]byte{0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae}

// stunTestTLV encodes one attribute with its 4-byte alignment padding.
func stunTestTLV(typ uint16, val []byte) []byte {
	out := make([]byte, 4, 4+len(val)+3)
	binary.BigEndian.PutUint16(out[0:2], typ)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(val)))
	out = append(out, val...)
	if pad := (4 - len(val)%4) % 4; pad > 0 {
		out = append(out, make([]byte, pad)...)
	}
	return out
}

// stunTestRaw frames body as a STUN message with a correct length field.
func stunTestRaw(typ uint16, txid [12]byte, body []byte) []byte {
	out := make([]byte, stunHeaderSize, stunHeaderSize+len(body))
	binary.BigEndian.PutUint16(out[0:2], typ)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(body)))
	binary.BigEndian.PutUint32(out[4:8], stunMagicCookie)
	copy(out[8:20], txid[:])
	return append(out, body...)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestSTUNEncodeParseRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		msg   stunMessage
		attrs int
	}{
		{
			name:  "bare request",
			msg:   stunMessage{Type: stunBindingRequest, TxID: rfc5769TxID},
			attrs: 0,
		},
		{
			name: "change request",
			msg: stunMessage{Type: stunBindingRequest, TxID: rfc5769TxID, Attrs: []stunAttr{
				{Type: stunAttrChangeRequest, Value: []byte{0, 0, 0, stunChangeIP | stunChangePort}},
			}},
			attrs: 1,
		},
		{
			name: "response with odd-length software",
			msg: stunMessage{Type: stunBindingSuccess, TxID: rfc5769TxID, Attrs: []stunAttr{
				{Type: stunAttrSoftware, Value: []byte("tslink/1")},
				{Type: stunAttrXORMappedAddress, Value: stunEncodeAddr(netip.MustParseAddrPort("192.0.2.1:32853"), true, rfc5769TxID)},
				{Type: stunAttrOtherAddress, Value: stunEncodeAddr(netip.MustParseAddrPort("198.51.100.7:3479"), false, rfc5769TxID)},
				{Type: 0x7fff, Value: []byte{1, 2, 3, 4, 5}}, // unknown, needs padding
			}},
			attrs: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.msg.encode()
			if len(raw)%4 != 0 {
				t.Fatalf("encoded message is not 4-byte aligned: %d", len(raw))
			}
			if got := binary.BigEndian.Uint32(raw[4:8]); got != stunMagicCookie {
				t.Fatalf("magic cookie = %#x", got)
			}
			got, err := parseSTUNMessage(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.Type != tc.msg.Type || got.TxID != tc.msg.TxID {
				t.Fatalf("header mismatch: got %#x/%x", got.Type, got.TxID)
			}
			if len(got.Attrs) != tc.attrs {
				t.Fatalf("attrs = %d, want %d", len(got.Attrs), tc.attrs)
			}
			for i, a := range tc.msg.Attrs {
				if got.Attrs[i].Type != a.Type {
					t.Errorf("attr %d type = %#x, want %#x", i, got.Attrs[i].Type, a.Type)
				}
				if !bytes.Equal(got.Attrs[i].Value, a.Value) {
					t.Errorf("attr %d value = %x, want %x", i, got.Attrs[i].Value, a.Value)
				}
			}
		})
	}
}

func TestSTUNDecodeXORMappedAddress(t *testing.T) {
	tests := []struct {
		name string
		attr uint16
		// hand-computed payload: reserved, family, xor-port, xor-address
		payload string
		want    string
	}{
		{
			// 192.0.2.1 ^ 2112a442 = e112a643, port 32853 ^ 0x2112 = 0xa147
			name:    "v4",
			attr:    stunAttrXORMappedAddress,
			payload: "0001a147e112a643",
			want:    "192.0.2.1:32853",
		},
		{
			// same, delivered under the legacy 0x8020 attribute type
			name:    "v4 legacy attr",
			attr:    stunAttrXORMappedAddrAlt,
			payload: "0001a147e112a643",
			want:    "192.0.2.1:32853",
		},
		{
			// 2001:db8:1234:5678:11:2233:4455:6677 ^ (cookie || txid)
			name:    "v6",
			attr:    stunAttrXORMappedAddress,
			payload: "0002a1470113a9faa5d3f179bc25f4b5bed2b9d9",
			want:    "[2001:db8:1234:5678:11:2233:4455:6677]:32853",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustHex(t, tc.payload)
			raw := stunTestRaw(stunBindingSuccess, rfc5769TxID, stunTestTLV(tc.attr, payload))
			msg, err := parseSTUNMessage(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, ok := msg.mappedAddr()
			if !ok {
				t.Fatal("no mapped address decoded")
			}
			if got.String() != tc.want {
				t.Fatalf("mapped = %s, want %s", got, tc.want)
			}
			// encoding it again must reproduce the same bytes
			if back := stunEncodeAddr(got, true, rfc5769TxID); !bytes.Equal(back, payload) {
				t.Fatalf("re-encoded = %x, want %x", back, payload)
			}
		})
	}
}

func TestSTUNDecodePlainMappedAddress(t *testing.T) {
	payload := mustHex(t, "00010d96c0000201") // 192.0.2.1:3478, no XOR
	raw := stunTestRaw(stunBindingSuccess, rfc5769TxID, stunTestTLV(stunAttrMappedAddress, payload))
	msg, err := parseSTUNMessage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := msg.mappedAddr()
	if !ok || got.String() != "192.0.2.1:3478" {
		t.Fatalf("mapped = %v (ok=%v), want 192.0.2.1:3478", got, ok)
	}
}

func TestSTUNParseTolerance(t *testing.T) {
	good := stunTestTLV(stunAttrXORMappedAddress, mustHex(t, "0001a147e112a643"))

	tests := []struct {
		name      string
		raw       []byte
		wantErr   bool
		wantAttrs int
		wantMap   string
	}{
		{
			name:      "unknown attributes are skipped",
			raw:       stunTestRaw(stunBindingSuccess, rfc5769TxID, concat(stunTestTLV(0x7f01, []byte{9}), good, stunTestTLV(0xfffe, []byte("xyz")))),
			wantAttrs: 3,
			wantMap:   "192.0.2.1:32853",
		},
		{
			name:      "missing trailing padding tolerated",
			raw:       stunTestRaw(stunBindingSuccess, rfc5769TxID, concat(good, []byte{0x80, 0x22, 0x00, 0x03, 'a', 'b', 'c'})),
			wantAttrs: 2,
			wantMap:   "192.0.2.1:32853",
		},
		{
			name:      "fingerprint after mapped address",
			raw:       stunTestRaw(stunBindingSuccess, rfc5769TxID, concat(good, stunTestTLV(stunAttrFingerprint, []byte{1, 2, 3, 4}))),
			wantAttrs: 2,
			wantMap:   "192.0.2.1:32853",
		},
		{
			name:    "header shorter than 20 bytes",
			raw:     []byte{0x01, 0x01, 0x00, 0x00},
			wantErr: true,
		},
		{
			name:    "trailing bytes beyond declared length ignored",
			raw:     append(stunTestRaw(stunBindingSuccess, rfc5769TxID, nil), 0x00),
			wantErr: false,
		},
		{
			name: "truncated attribute value",
			raw: func() []byte {
				b := stunTestRaw(stunBindingSuccess, rfc5769TxID, []byte{0x00, 0x20, 0x00, 0x10, 0x00, 0x01})
				return b
			}(),
			wantErr: true,
		},
		{
			name:    "truncated attribute header",
			raw:     stunTestRaw(stunBindingSuccess, rfc5769TxID, []byte{0x00, 0x20, 0x00}),
			wantErr: true,
		},
		{
			name:      "address attribute shorter than its family requires",
			raw:       stunTestRaw(stunBindingSuccess, rfc5769TxID, stunTestTLV(stunAttrXORMappedAddress, mustHex(t, "0002a1470113a9fa"))),
			wantAttrs: 1,
			wantMap:   "", // v6 payload truncated: reported as absent, not fatal
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := parseSTUNMessage(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.wantAttrs != 0 && len(msg.Attrs) != tc.wantAttrs {
				t.Fatalf("attrs = %d, want %d", len(msg.Attrs), tc.wantAttrs)
			}
			got, ok := msg.mappedAddr()
			if tc.wantMap == "" {
				if ok {
					t.Fatalf("expected no mapped address, got %s", got)
				}
				return
			}
			if !ok || got.String() != tc.wantMap {
				t.Fatalf("mapped = %v (ok=%v), want %s", got, ok, tc.wantMap)
			}
		})
	}
}

func TestSTUNResponseFor(t *testing.T) {
	other := [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	body := stunTestTLV(stunAttrXORMappedAddress, mustHex(t, "0001a147e112a643"))

	tests := []struct {
		name string
		raw  []byte
		txid [12]byte
		want bool
	}{
		{"matching success", stunTestRaw(stunBindingSuccess, rfc5769TxID, body), rfc5769TxID, true},
		{"matching error response", stunTestRaw(stunBindingError, rfc5769TxID, nil), rfc5769TxID, true},
		{"txid mismatch", stunTestRaw(stunBindingSuccess, other, body), rfc5769TxID, false},
		{"request is not a response", stunTestRaw(stunBindingRequest, rfc5769TxID, nil), rfc5769TxID, false},
		{"garbage", []byte("not a stun packet"), rfc5769TxID, false},
		{"empty", nil, rfc5769TxID, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := stunResponseFor(tc.raw, tc.txid)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && msg == nil {
				t.Fatal("accepted response but returned nil message")
			}
		})
	}
}

func TestSTUNBindingRequestMsg(t *testing.T) {
	plain := stunBindingRequestMsg(0)
	if len(plain.Attrs) != 0 {
		t.Fatalf("plain request carries %d attributes", len(plain.Attrs))
	}
	if plain.TxID == ([12]byte{}) {
		t.Fatal("transaction id was not randomised")
	}
	if other := stunBindingRequestMsg(0); other.TxID == plain.TxID {
		t.Fatal("two requests share a transaction id")
	}
	cr := stunBindingRequestMsg(stunChangeIP | stunChangePort)
	v, ok := cr.attr(stunAttrChangeRequest)
	if !ok || len(v) != 4 || v[3] != 0x06 {
		t.Fatalf("change-request attribute = %x (ok=%v)", v, ok)
	}
}

func TestSTUNServerLists(t *testing.T) {
	var cn, intl int
	hosts := map[string]bool{}
	for _, s := range DefaultSTUNServers() {
		if hosts[s.Host] {
			t.Errorf("duplicate host %s", s.Host)
		}
		hosts[s.Host] = true
		if s.Name == "" {
			t.Errorf("%s has no name", s.Host)
		}
		switch s.Region {
		case RegionCN:
			cn++
		case RegionIntl:
			intl++
		default:
			t.Errorf("%s has unknown region %q", s.Host, s.Region)
		}
	}
	if cn == 0 || intl == 0 {
		t.Fatalf("default list must span both regions, got cn=%d intl=%d", cn, intl)
	}
	for _, s := range RFC5780Servers() {
		if !hosts[s.Host] {
			t.Errorf("rfc5780 server %s missing from the default list", s.Host)
		}
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
