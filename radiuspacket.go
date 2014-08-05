package goradius

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
)

var (
	HEADER_SIZE = 20
)

type RadiusHeader struct {
	Code          uint8
	Identifier    uint8
	Length        uint16
	Authenticator [authenticatorLength]byte
}

type RadiusAttribute struct {
	Type   uint8
	Length uint8
	Value  []byte
}

type RadiusPacket struct {
	RadiusHeader
	Attributes []RadiusAttribute
	Addr       *net.UDPAddr
}

type VendorSpecificAttribute struct {
	VendorId     uint32
	VendorType   uint8
	VendorLength uint8
}

/*
 * RadiusHeader
 */

func (r RadiusHeader) String() string {
	return fmt.Sprintf("Type: '%v' Identifier: %v Length: %v Authenticator: %x",
		request_type_to_string[r.Code], r.Identifier, r.Length, r.Authenticator)
}

/*
 * RadiusAttribute
 */

func (r RadiusAttribute) Bytes() []byte {

	buf := bytes.NewBuffer([]byte{})

	err := binary.Write(buf, binary.BigEndian, &r.Type)
	if err != nil {
		log.Fatal(err)
	}

	r.Length = uint8(len(r.Value) + 2)
	err = binary.Write(buf, binary.BigEndian, &r.Length)
	if err != nil {
		log.Fatal(err)
	}

	err = binary.Write(buf, binary.BigEndian, r.Value[:])
	if err != nil {
		log.Fatal(err)
	}

	return buf.Bytes()

}

func (r RadiusAttribute) String() string {
	return fmt.Sprintf("%v: %x", code_to_attributes[r.Type], r.Value)
}

/*
 * RadiusPacket
 */

func NewRadiusPacket() *RadiusPacket {
	var p RadiusPacket
	return &p

}

func (r RadiusPacket) String() string {
	return fmt.Sprintf("RadiusPacket{%v %v}", r.RadiusHeader, r.Attributes)
}

func (r *RadiusPacket) Duplicate() *RadiusPacket {

	dest := RadiusPacket{}
	dest.Code = r.Code
	dest.Identifier = r.Identifier
	dest.Length = r.Length
	dest.Authenticator = r.Authenticator

	for _, attr := range r.Attributes {
		dest.Attributes = append(dest.Attributes, attr)
	}

	return &dest
}

func (p *RadiusPacket) AddAttribute(attrTypeStr string, value []byte) error {

	var err error
	if attrTypeCode, ok := attributes_to_code[attrTypeStr]; ok {
		attr := RadiusAttribute{
			Type:  attrTypeCode,
			Value: value,
		}

		p.Attributes = append(p.Attributes, attr)
		err = nil
	} else {
		err = errors.New("Attribute not found")
	}

	return err
}

func (p *RadiusPacket) AddAttributeByType(attrType uint8, value []byte) {

	attr := RadiusAttribute{
		Type:  attrType,
		Value: value,
	}

	p.Attributes = append(p.Attributes, attr)

}

func (p *RadiusPacket) GetAttribute(attrType string) [][]byte {

	var attrs [][]byte

	if attrTypeCode, ok := attributes_to_code[attrType]; ok {
		for _, v := range p.Attributes {

			if v.Type == attrTypeCode {

				if v.Type == VendorSpecific {

					reader := bytes.NewBuffer(v.Value)

					vsa := VendorSpecificAttribute{}
					err := binary.Read(reader, binary.BigEndian, &vsa)
					if err != nil {
						log.Fatal(err)
					}

					value := reader.Next(int(vsa.VendorLength))
					attrs = append(attrs, value)
				} else {
					attrs = append(attrs, v.Value)
				}

			}

		}
	}

	return attrs
}

func (p *RadiusPacket) GetFirstAttribute(attrType string) []byte {

	var attr []byte
	attrs := p.GetAttribute(attrType)
	if len(attrs) > 0 {
		attr = attrs[0]
	}

	return attr
}

func (p *RadiusPacket) GetFirstAttributeAsString(attrType string) string {
	return string(p.GetFirstAttribute(attrType))
}

func (r *RadiusPacket) encodeAttrs(secret string) []byte {

	buf := bytes.NewBuffer([]byte{})

	for _, attr := range r.Attributes {

		switch attr.Type {
		case UserPassword:
			// We usually wanna decode the password because if we proxy it
			// we will need to re-encode with the new secret  anyways
			password_data := encryptPassword(secret, r.Authenticator, attr.Value)
			attr.Length = uint8(len(password_data))
			attr.Value = password_data[:]
		default:
			attr.Length = uint8(len(attr.Value))
		}

		buf.Write(attr.Bytes())
	}

	return buf.Bytes()
}

func (r *RadiusPacket) EncodePacket(secret string) ([]byte, error) {

	// encode all attrs first
	attrs_data := r.encodeAttrs(secret)
	attrs_size := len(attrs_data)
	r.Length = uint16(attrs_size + HEADER_SIZE)

	buf := bytes.NewBuffer([]byte{})

	err := binary.Write(buf, binary.BigEndian, &r.RadiusHeader)
	if err != nil {
		return nil, err
	}

	err = binary.Write(buf, binary.BigEndian, attrs_data)

	return buf.Bytes(), err

}

func ParseRADIUSPacket(rawMsg []byte, secret string) (*RadiusPacket, error) {

	packet := NewRadiusPacket()
	reader := bytes.NewReader(rawMsg)

	err := binary.Read(reader, binary.BigEndian, &packet.RadiusHeader)
	if err != nil {
		return nil, err
	}

	rawAttributesBytes := rawMsg[headerEnd:]

	rawAttributes := parseAttributes(rawAttributesBytes, packet.Authenticator, secret)

	for _, attr := range rawAttributes {
		name := code_to_attributes[attr.Type]
		packet.AddAttribute(name, attr.Value)
	}

	return packet, nil

}

func parseAttributes(data []byte, requestAuthenticator [16]byte, secret string) []RadiusAttribute {

	var attrs []RadiusAttribute
	reader := bytes.NewBuffer(data)

	for {

		var e error
		var attr_type uint8
		var length uint8
		var value []byte
		ok := false

		attr_type, e = reader.ReadByte()
		if e == io.EOF {
			break
		}

		length, e = reader.ReadByte()
		if e == io.EOF {
			break
		}

		switch attr_type {
		case uint8(0):
			log.Printf("attr_type 0?")
		case uint8(UserPassword):
			value = reader.Next(int(length) - 2)
			value = decryptPassword(secret, value, requestAuthenticator)
			ok = true
		// case uint8(26):
		// 	vsa := VendorSpecificAttribute{}
		// 	e = binary.Read(reader, binary.BigEndian, &vsa)
		// 	if e != nil {
		// 		log.Fatal(e)
		// 	}
		// 	value = reader.Next(int(vsa.VendorLength))
		// 	ok = true
		// 	log.Printf("VSA: %+v", vsa)
		// 	log.Printf("Venue-Id: %v", string(value))
		default:
			value = reader.Next(int(length) - 2)
			ok = true
		}

		if ok {
			attr := RadiusAttribute{
				Type:   attr_type,
				Length: length,
				Value:  value,
			}

			attrs = append(attrs, attr)
		}

	}

	return attrs
}

func GenerateRandomAuthenticator() [16]byte {

	authenticator := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	_, err := rand.Read(authenticator[:])
	if err != nil {
		panic(err)
	}

	return authenticator
}

func paddAttr(data []byte, size int) []byte {

	padded := make([]byte, size)
	for i, b := range data {
		padded[i] = b
	}
	return padded

}

func encryptPassword(secret string, authenticator [16]byte, password []byte) [16]byte {

	paddedPassword := paddAttr(password, 16)

	_b := md5.New()
	_b.Write([]byte(secret))
	_b.Write(authenticator[:])
	b := _b.Sum(nil)

	xored := [16]byte{}

	for i := 0; i < 16; i++ {
		xored[i] = paddedPassword[i] ^ b[i]
	}

	return xored
}

func decryptPassword(secret string, value []byte, requestAuthenticator [16]byte) []byte {

	// TODO: Allow passwords longer than 16 characters...
	var bufr [16]byte

	S := []byte(secret)
	c := requestAuthenticator[0:16]

	_b := md5.New()
	_b.Write(S)
	_b.Write(c)
	b := _b.Sum(nil)

	for i, p := range value {
		bufr[i] = b[i] ^ p
	}

	idx := strings.Index(string(bufr[0:16]), "\x00")
	ret := bufr[0:16]
	if idx > 0 {
		ret = bufr[:idx]
	}
	return ret
}
