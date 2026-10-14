package main

import (
	"encoding/binary"
	"strconv"
	"testing"
)

func orientationEXIF(order binary.ByteOrder, orientation uint16) []byte {
	tiff := make([]byte, 26)
	if order == binary.LittleEndian {
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	order.PutUint16(tiff[2:], 42)
	order.PutUint32(tiff[4:], 8)
	order.PutUint16(tiff[8:], 1)
	order.PutUint16(tiff[10:], 0x0112)
	order.PutUint16(tiff[12:], 3)
	order.PutUint32(tiff[14:], 1)
	order.PutUint16(tiff[18:], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0, byte(len(payload) + 2)}
	return append(jpeg, payload...)
}

func TestJPEGOrientation(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for orientation := 1; orientation <= 8; orientation++ {
			data := orientationEXIF(order, uint16(orientation))
			if got := jpegOrientation(data); got != orientation {
				t.Fatalf("orientation=%d, got %d", orientation, got)
			}
		}
	}
}

func TestJPEGOrientationMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		{0xff, 0xd8, 0xff, 0xe1, 0, 0},
		{0xff, 0xd8, 0xff, 0xe1, 0, 1},
		{0xff, 0xd8, 0xff, 0xe1, 0, 20},
		{0xff, 0xd8, 0xff, 0xd9},
	}
	valid := orientationEXIF(binary.LittleEndian, 6)
	// Every truncated prefix should be safe, including a complete orientation
	// entry whose enclosing segment is shorter than its declared length.
	for i := 0; i < len(valid); i++ {
		cases = append(cases, valid[:i])
	}
	for i, data := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := jpegOrientation(data); got != 1 {
				t.Fatalf("malformed input returned %d, want default 1", got)
			}
		})
	}
}

func FuzzJPEGOrientation(f *testing.F) {
	f.Add(orientationEXIF(binary.LittleEndian, 6))
	f.Add(orientationEXIF(binary.BigEndian, 8))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if got := jpegOrientation(data); got < 1 || got > 8 {
			t.Fatalf("orientation out of range: %d", got)
		}
	})
}
