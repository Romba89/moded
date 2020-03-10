// Package dns deals with encoding and decoding DNS wire format.
package dns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// The maximum number of DNS name compression pointers we are willing to follow.
// Without something like this, infinite loops are possible.
const compressionPointerLimit = 10

var (
	// ErrZeroLengthLabel is the error returned for names that contain a
	// zero-length label, like "example..com".
	ErrZeroLengthLabel = errors.New("name contains a zero-length label")

	// ErrLabelTooLong is the error returned for labels that are longer than
	// 63 octets.
	ErrLabelTooLong = errors.New("name contains a label longer than 63 octets")

	// ErrNameTooLong is the error returned for names whose encoded
	// representation is longer than 255 octets.
	ErrNameTooLong = errors.New("name is longer than 255 octets")

	// ErrReservedLabelType is the error returned when reading a label type
	// prefix whose two most significant bits are not 00 or 11.
	ErrReservedLabelType = errors.New("reserved label type")

	// ErrTooManyPointers is the error returned when reading a compressed
	// name that has too many compression pointers.
	ErrTooManyPointers = errors.New("too many compression pointers")

	// ErrTrailingBytes is the error returned when bytes remain in the parse
	// buffer after parsing a message.
	ErrTrailingBytes = errors.New("trailing bytes after message")
)

// Name represents a domain name, a sequence of labels each of which is 63
// octets or less in length.
//
// https://tools.ietf.org/html/rfc1035#section-3.1
type Name [][]byte

// NewName returns a Name from a slice of labels, after checking the labels for
// validity. Do not include a zero-length label at the end of the slice.
func NewName(labels [][]byte) (Name, error) {
	name := Name(labels)
	// https://tools.ietf.org/html/rfc1035#section-2.3.4
	// Various objects and parameters in the DNS have size limits.
	//   labels          63 octets or less
	//   names           255 octets or less
	totalLen := 1 // "...a domain name is terminated by a length byte of zero."
	for _, label := range labels {
		if len(label) == 0 {
			return nil, ErrZeroLengthLabel
		}
		if len(label) > 63 {
			return nil, ErrLabelTooLong
		}
		totalLen += 1 + len(label)
		if totalLen > 255 {
			return nil, ErrNameTooLong
		}
	}
	return name, nil
}

// ParseName returns a new Name from a string of labels separated by dots, after
// checking the name for validity. A single dot at the end of the string is
// ignored.
func ParseName(s string) (Name, error) {
	b := bytes.TrimSuffix([]byte(s), []byte("."))
	if len(b) == 0 {
		// bytes.Split(b, ".") would return [""] in this case
		return NewName([][]byte{})
	} else {
		return NewName(bytes.Split(b, []byte(".")))
	}
	return NewName(bytes.Split(b, []byte(".")))
}

// String returns a string representation of name, with labels separated by
// dots.
func (name Name) String() string {
	if len(name) == 0 {
		return "."
	} else {
		return string(bytes.Join(name, []byte(".")))
	}
}

// Message represents a DNS message.
//
// https://tools.ietf.org/html/rfc1035#section-4.1
type Message struct {
	ID    uint16
	Flags uint16

	Question   []Question
	Answer     []RR
	Authority  []RR
	Additional []RR
}

// Question represents the question section of a message.
//
// https://tools.ietf.org/html/rfc1035#section-4.1.2
type Question struct {
	Name  Name
	Type  uint16
	Class uint16
}

// RR represents a resource record.
//
// https://tools.ietf.org/html/rfc1035#section-4.1.3
type RR struct {
	Name  Name
	Type  uint16
	Class uint16
	TTL   uint32
	Data  []byte
}

func readName(r io.ReadSeeker) (Name, error) {
	var labels [][]byte
	// We limit the number of compression pointers we are willing to follow.
	numPointers := 0
	// If we followed any compression pointers, we must finally seek to just
	// past the first pointer.
	var seekTo int64
loop:
	for {
		var labelType byte
		err := binary.Read(r, binary.BigEndian, &labelType)
		if err != nil {
			return nil, err
		}

		switch labelType & 0xc0 {
		case 0x00:
			// This is an ordinary label.
			// https://tools.ietf.org/html/rfc1035#section-3.1
			length := int(labelType & 0x3f)
			if length == 0 {
				break loop
			}
			label := make([]byte, length)
			_, err := io.ReadFull(r, label)
			if err != nil {
				return nil, err
			}
			labels = append(labels, label)
		case 0xc0:
			// This is a compression pointer.
			// https://tools.ietf.org/html/rfc1035#section-4.1.4
			upper := labelType & 0x3f
			var lower byte
			err := binary.Read(r, binary.BigEndian, &lower)
			if err != nil {
				return nil, err
			}
			offset := (uint16(upper) << 8) | uint16(lower)

			if numPointers == 0 {
				// The first time we encounter a pointer,
				// remember our position so we can seek back to
				// it when done.
				seekTo, err = r.Seek(0, io.SeekCurrent)
				if err != nil {
					return nil, err
				}
			}
			numPointers++
			if numPointers > compressionPointerLimit {
				return nil, ErrTooManyPointers
			}

			// Follow the pointer and continue.
			_, err = r.Seek(int64(offset), io.SeekStart)
			if err != nil {
				return nil, err
			}
		default:
			// "The 10 and 01 combinations are reserved for future
			// use."
			return nil, ErrReservedLabelType
		}
	}
	// If we followed any pointers, then seek back to just after the first
	// one.
	if numPointers > 0 {
		_, err := r.Seek(seekTo, io.SeekStart)
		if err != nil {
			return nil, err
		}
	}
	return NewName(labels)
}

func readQuestion(r io.ReadSeeker) (Question, error) {
	var question Question

	// https://tools.ietf.org/html/rfc1035#section-4.1.2
	var err error
	question.Name, err = readName(r)
	if err != nil {
		return question, err
	}
	for _, ptr := range []*uint16{&question.Type, &question.Class} {
		err := binary.Read(r, binary.BigEndian, ptr)
		if err != nil {
			return question, err
		}
	}

	return question, nil
}

func readRR(r io.ReadSeeker) (RR, error) {
	var rr RR

	// https://tools.ietf.org/html/rfc1035#section-4.1.3
	var err error
	rr.Name, err = readName(r)
	if err != nil {
		return rr, err
	}
	for _, ptr := range []*uint16{&rr.Type, &rr.Class} {
		err := binary.Read(r, binary.BigEndian, ptr)
		if err != nil {
			return rr, err
		}
	}
	err = binary.Read(r, binary.BigEndian, &rr.TTL)
	if err != nil {
		return rr, err
	}
	var rdLength uint16
	err = binary.Read(r, binary.BigEndian, &rdLength)
	if err != nil {
		return rr, err
	}
	rr.Data = make([]byte, rdLength)
	_, err = io.ReadFull(r, rr.Data)
	if err != nil {
		return rr, err
	}

	return rr, nil
}

func readMessage(r io.ReadSeeker) (Message, error) {
	var message Message

	// Header section
	// https://tools.ietf.org/html/rfc1035#section-4.1.1
	var qdCount, anCount, nsCount, arCount uint16
	for _, ptr := range []*uint16{
		&message.ID, &message.Flags,
		&qdCount, &anCount, &nsCount, &arCount,
	} {
		err := binary.Read(r, binary.BigEndian, ptr)
		if err != nil {
			return message, err
		}
	}

	// Question section
	// https://tools.ietf.org/html/rfc1035#section-4.1.2
	for i := 0; i < int(qdCount); i++ {
		question, err := readQuestion(r)
		if err != nil {
			return message, err
		}
		message.Question = append(message.Question, question)
	}

	// Answer, Authority, and Additional sections
	// https://tools.ietf.org/html/rfc1035#section-4.1.3
	for _, rec := range []struct {
		ptr   *[]RR
		count uint16
	}{
		{&message.Answer, anCount},
		{&message.Authority, nsCount},
		{&message.Additional, arCount},
	} {
		for i := 0; i < int(rec.count); i++ {
			rr, err := readRR(r)
			if err != nil {
				return message, err
			}
			*rec.ptr = append(*rec.ptr, rr)
		}
	}

	// Check for trailing bytes.
	var buf [1]byte
	_, err := io.ReadFull(r, buf[:])
	if err == nil {
		err = ErrTrailingBytes
	}
	if err != io.EOF {
		return message, err
	}

	return message, nil
}

// MessageFromWireFormat parses a message from a buffer of bytes and returns a
// Message object.
func MessageFromWireFormat(buf []byte) (Message, error) {
	message, err := readMessage(bytes.NewReader(buf))
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return message, err
}