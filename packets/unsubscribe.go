package packets

import (
	"bytes"
	"fmt"
	"io"
	"net"
)

// Unsubscribe is the Variable Header definition for a Unsubscribe control packet
type Unsubscribe struct {
	Topics     []string
	Properties *Properties
	PacketID   uint16
}

func (u *Unsubscribe) String() string {
	if isVer4() {
		return fmt.Sprintf("UNSUBSCRIBE: PacketID:%d Topics:%v\n", u.PacketID, u.Topics)
	} else {
		return fmt.Sprintf("UNSUBSCRIBE: PacketID:%d Topics:%v Properties:\n%s", u.PacketID, u.Topics, u.Properties)
	}
}

// Unpack is the implementation of the interface required function for a packet
func (u *Unsubscribe) Unpack(r *bytes.Buffer) error {
	var err error
	u.PacketID, err = readUint16(r)
	if err != nil {
		return err
	}

	err = genPropPack(UNSUBSCRIBE).Unpack(r, u.Properties)
	if err != nil {
		return err
	}

	for {
		t, err := readString(r)
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF {
			break
		}
		u.Topics = append(u.Topics, t)
	}

	return nil
}

// Buffers is the implementation of the interface required function for a packet
func (u *Unsubscribe) Buffers() net.Buffers {
	var b bytes.Buffer
	writeUint16(u.PacketID, &b)
	var topics bytes.Buffer
	for _, t := range u.Topics {
		writeString(t, &topics)
	}
	var propBuf bytes.Buffer
	genPropPack(UNSUBSCRIBE).Pack(u.Properties, &propBuf)
	return net.Buffers{b.Bytes(), propBuf.Bytes(), topics.Bytes()}
}

// WriteTo is the implementation of the interface required function for a packet
func (u *Unsubscribe) WriteTo(w io.Writer) (int64, error) {
	cp := &ControlPacket{FixedHeader: FixedHeader{Type: UNSUBSCRIBE, Flags: 2}}
	cp.Content = u

	return cp.WriteTo(w)
}
