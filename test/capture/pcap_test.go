package capture

import (
	"encoding/binary"
	"os"
	"testing"
)

// Direction of one reassembled TCP stream.
type Direction string

const (
	// ToStation is the controlling station (our side) talking to the RTU.
	ToStation Direction = "M->S"

	// ToMaster is the RTU answering.
	ToMaster Direction = "S->M"
)

// segment is one TCP payload, in capture order.
type segment struct {
	Dir Direction

	// Conn identifies the TCP connection the payload belongs to, as the client
	// port number. Streams from different connections must never be spliced
	// together: a capture taken against a simulator often holds several
	// sessions, and merging them produces a byte stream that desynchronises in
	// a way that looks exactly like a framing bug.
	Conn uint16

	Payload []byte
}

// readCapture parses a classic libpcap file and returns the TCP payloads
// carried to and from port 2404, in capture order.
//
// This is a deliberately small reader rather than a dependency: it handles
// exactly the one link type and the one encapsulation this capture uses, and
// says so loudly when given anything else. Pulling in a packet-capture library
// to read ten kilobytes of test fixture would be a poor trade — and this package
// is test-only, so the dependency would sit in go.mod for everyone.
//
// It also assumes the capture has no retransmissions or reordering, which holds
// for a clean single-connection capture and is asserted indirectly: any gap
// would desynchronise the frame walk immediately and loudly.
func readCapture(t *testing.T, path string) []segment {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the capture: %v", err)
	}
	if len(data) < 24 {
		t.Fatalf("%s is too short to be a capture file", path)
	}

	var out []segment
	switch {
	case binary.LittleEndian.Uint32(data[0:4]) == 0x0A0D0D0A:
		out = readPcapNG(t, path, data)
	case binary.LittleEndian.Uint32(data[0:4]) == 0xA1B2C3D4:
		out = readClassicPcap(t, path, data)
	default:
		t.Fatalf("%s: magic 0x%08X is neither a little-endian microsecond pcap nor a pcapng; "+
			"re-save it from Wireshark as pcap or pcapng", path, binary.LittleEndian.Uint32(data[0:4]))
	}

	if len(out) == 0 {
		t.Fatalf("%s carries no TCP payload on port 2404", path)
	}
	return out
}

// readClassicPcap reads the original libpcap format: a 24-octet header, then a
// 16-octet record header before each packet.
func readClassicPcap(t *testing.T, path string, data []byte) []segment {
	t.Helper()

	linkType := binary.LittleEndian.Uint32(data[20:24])
	var out []segment

	off := 24
	for off+16 <= len(data) {
		inclLen := int(binary.LittleEndian.Uint32(data[off+8 : off+12]))
		off += 16
		if off+inclLen > len(data) {
			t.Fatalf("%s: truncated packet record at offset %d", path, off)
		}
		if seg, ok := tcpPayload(t, path, linkType, data[off:off+inclLen]); ok {
			out = append(out, seg)
		}
		off += inclLen
	}
	return out
}

// readPcapNG reads the block-structured format Wireshark now writes by default.
//
// Only the three block types a plain capture contains are understood: the
// section header, the interface description (which carries the link type) and
// the enhanced packet block. Everything else is skipped by its length, which is
// what the format is designed for.
func readPcapNG(t *testing.T, path string, data []byte) []segment {
	t.Helper()

	const (
		blockSectionHeader = 0x0A0D0D0A
		blockInterfaceDesc = 0x00000001
		blockEnhancedPkt   = 0x00000006
	)

	var (
		out       []segment
		linkTypes []uint32
	)

	off := 0
	for off+12 <= len(data) {
		blockType := binary.LittleEndian.Uint32(data[off : off+4])
		blockLen := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))

		if blockType == blockSectionHeader {
			// The byte-order magic decides the endianness of the whole section.
			// Only little-endian is handled: every capture this project has
			// seen is written by a little-endian host, and quietly misreading a
			// big-endian one would be worse than refusing it.
			if bom := binary.LittleEndian.Uint32(data[off+8 : off+12]); bom != 0x1A2B3C4D {
				t.Fatalf("%s: byte-order magic 0x%08X; only little-endian pcapng is handled", path, bom)
			}
		}
		if blockLen < 12 || off+blockLen > len(data) {
			t.Fatalf("%s: block at offset %d claims %d octets", path, off, blockLen)
		}
		body := data[off+8 : off+blockLen-4]

		switch blockType {
		case blockInterfaceDesc:
			if len(body) < 8 {
				t.Fatalf("%s: short interface description block", path)
			}
			linkTypes = append(linkTypes, uint32(binary.LittleEndian.Uint16(body[0:2])))

		case blockEnhancedPkt:
			if len(body) < 20 {
				t.Fatalf("%s: short enhanced packet block", path)
			}
			iface := int(binary.LittleEndian.Uint32(body[0:4]))
			capLen := int(binary.LittleEndian.Uint32(body[12:16]))
			if iface >= len(linkTypes) {
				t.Fatalf("%s: packet references interface %d before its description", path, iface)
			}
			if 20+capLen > len(body) {
				t.Fatalf("%s: packet claims %d octets, block holds %d", path, capLen, len(body)-20)
			}
			if seg, ok := tcpPayload(t, path, linkTypes[iface], body[20:20+capLen]); ok {
				out = append(out, seg)
			}
		}

		off += blockLen
	}
	return out
}

// tcpPayload peels Ethernet, IPv4 and TCP off one packet.
func tcpPayload(t *testing.T, path string, linkType uint32, pkt []byte) (segment, bool) {
	t.Helper()

	const (
		linkNull     = 0 // BSD/Npcap loopback: a 4-octet address family, then IP
		linkEthernet = 1
		ethHeaderLen = 14
		etherTypeIP4 = 0x0800
		protoTCP     = 6
		port104      = 2404
	)

	var ipOff int
	switch linkType {
	case linkEthernet:
		if len(pkt) < ethHeaderLen {
			return segment{}, false
		}
		if binary.BigEndian.Uint16(pkt[12:14]) != etherTypeIP4 {
			return segment{}, false
		}
		ipOff = ethHeaderLen

	case linkNull:
		// A capture taken on the loopback interface. The four octets are the
		// address family in host order; only IPv4 is of interest here and the
		// IP version nibble below rejects anything else anyway.
		if len(pkt) < 4 {
			return segment{}, false
		}
		ipOff = 4

	default:
		t.Fatalf("%s: link type %d is neither Ethernet nor loopback", path, linkType)
	}

	ip := pkt[ipOff:]
	if len(ip) < 20 {
		return segment{}, false
	}
	if ip[0]>>4 != 4 {
		return segment{}, false
	}
	ihl := int(ip[0]&0x0F) * 4
	if ihl < 20 || len(ip) < ihl || ip[9] != protoTCP {
		return segment{}, false
	}
	// The IP total length is authoritative: Ethernet pads short frames, and
	// trusting the captured length instead appends padding octets to the stream,
	// which desynchronises the frame walk a few frames later in a way that looks
	// like a parser bug.
	totalLen := int(binary.BigEndian.Uint16(ip[2:4]))
	if totalLen < ihl || totalLen > len(ip) {
		return segment{}, false
	}

	tcp := ip[ihl:totalLen]
	if len(tcp) < 20 {
		return segment{}, false
	}
	dataOff := int(tcp[12]>>4) * 4
	if dataOff < 20 || len(tcp) < dataOff {
		return segment{}, false
	}

	payload := tcp[dataOff:]
	if len(payload) == 0 {
		return segment{}, false
	}

	srcPort := binary.BigEndian.Uint16(tcp[0:2])
	dstPort := binary.BigEndian.Uint16(tcp[2:4])
	switch {
	case dstPort == port104:
		return segment{Dir: ToStation, Conn: srcPort, Payload: payload}, true
	case srcPort == port104:
		return segment{Dir: ToMaster, Conn: dstPort, Payload: payload}, true
	default:
		return segment{}, false
	}
}

func (d Direction) String() string { return string(d) }

// other returns the opposite direction.
func (d Direction) other() Direction {
	if d == ToStation {
		return ToMaster
	}
	return ToStation
}
