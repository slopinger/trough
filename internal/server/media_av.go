package server

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Video and audio uploads. There is no transcoding (trough is a single Go
// binary without ffmpeg), so only formats browsers play directly are taken:
// MP4/MOV (H.264/AAC from phones), WebM, MP3, Ogg, WAV and FLAC, recognised
// by their file signature. MP4 metadata boxes (udta/meta, where phones put
// the recording location) are blanked in place as "free" boxes, which keeps
// every sample offset valid.

const maxMediaSize = 40 << 20

var errUnsupportedMedia = errors.New("unsupported file type (use JPEG, PNG, GIF, WebP, MP4, WebM, MP3, Ogg, WAV or FLAC)")

type avInfo struct {
	mediaType, ext string
	width, height  int
}

// sniffAV recognises a video or audio file and cleans its metadata.
func sniffAV(data []byte) (*avInfo, error) {
	switch {
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		info := &avInfo{mediaType: "video/mp4", ext: ".mp4"}
		if brand := string(data[8:12]); brand == "M4A " || brand == "M4B " {
			info.mediaType, info.ext = "audio/mp4", ".m4a"
		}
		if !mp4Valid(data) {
			return nil, errors.New("broken MP4 file")
		}
		mp4StripMetadata(data)
		info.width, info.height = mp4Dimensions(data)
		if info.width == 0 && info.mediaType == "video/mp4" && !mp4HasVideo(data) {
			info.mediaType, info.ext = "audio/mp4", ".m4a"
		}
		return info, nil
	case bytes.HasPrefix(data, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		head := data[:min(len(data), 1<<16)]
		if !bytes.Contains(head, []byte("webm")) {
			return nil, errors.New("only WebM, not other Matroska files")
		}
		if bytes.Contains(head, []byte("V_VP")) || bytes.Contains(head, []byte("V_AV1")) {
			return &avInfo{mediaType: "video/webm", ext: ".webm"}, nil
		}
		return &avInfo{mediaType: "audio/webm", ext: ".weba"}, nil
	case bytes.HasPrefix(data, []byte("ID3")) || len(data) > 2 && data[0] == 0xFF && data[1]&0xE6 == 0xE2:
		return &avInfo{mediaType: "audio/mpeg", ext: ".mp3"}, nil
	case bytes.HasPrefix(data, []byte("OggS")):
		return &avInfo{mediaType: "audio/ogg", ext: ".ogg"}, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return &avInfo{mediaType: "audio/wav", ext: ".wav"}, nil
	case bytes.HasPrefix(data, []byte("fLaC")):
		return &avInfo{mediaType: "audio/flac", ext: ".flac"}, nil
	}
	return nil, errUnsupportedMedia
}

// mp4Boxes calls f for each box in b with its type, where the box starts,
// where its payload starts and where it ends.
func mp4Boxes(b []byte, f func(typ string, box, start, end int) bool) bool {
	for i := 0; i < len(b); {
		if i+8 > len(b) {
			return false
		}
		size, hdr := int(binary.BigEndian.Uint32(b[i:])), 8
		switch size {
		case 0: // to the end of the file
			size = len(b) - i
		case 1:
			if i+16 > len(b) {
				return false
			}
			s := binary.BigEndian.Uint64(b[i+8:])
			if s > uint64(len(b)-i) {
				return false
			}
			size, hdr = int(s), 16
		}
		if size < hdr || size > len(b)-i {
			return false
		}
		if !f(string(b[i+4:i+8]), i, i+hdr, i+size) {
			return true
		}
		i += size
	}
	return true
}

// mp4Valid checks that the top-level boxes are well formed and there is
// a movie header and media data.
func mp4Valid(data []byte) bool {
	var moov, mdat bool
	ok := mp4Boxes(data, func(typ string, _, _, _ int) bool {
		moov = moov || typ == "moov"
		mdat = mdat || typ == "mdat"
		return true
	})
	return ok && moov && mdat
}

var mp4Containers = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "edts": true}

// mp4StripMetadata turns metadata boxes inside moov into zeroed free boxes.
func mp4StripMetadata(data []byte) {
	var walk func(start, end int)
	walk = func(start, end int) {
		mp4Boxes(data[start:end], func(typ string, box, s, e int) bool {
			box, s, e = box+start, s+start, e+start
			switch {
			case typ == "udta" || typ == "meta" || typ == "uuid":
				copy(data[box+4:box+8], "free")
				clear(data[s:e])
			case mp4Containers[typ]:
				walk(s, e)
			}
			return true
		})
	}
	mp4Boxes(data, func(typ string, _, s, e int) bool {
		if typ == "moov" {
			walk(s, e)
		}
		return true
	})
}

// mp4Each calls f with the payload of every box along path, e.g. moov/trak/tkhd.
func mp4Each(data []byte, path []string, f func(payload []byte)) {
	mp4Boxes(data, func(typ string, _, s, e int) bool {
		if typ == path[0] {
			if len(path) == 1 {
				f(data[s:e])
			} else {
				mp4Each(data[s:e], path[1:], f)
			}
		}
		return true
	})
}

// mp4Dimensions reads the display size from the first video track header.
func mp4Dimensions(data []byte) (w, h int) {
	mp4Each(data, []string{"moov", "trak", "tkhd"}, func(p []byte) {
		if w > 0 || len(p) < 84 {
			return
		}
		off := 76 // version 0
		if p[0] == 1 {
			off = 88
		}
		if len(p) < off+8 {
			return
		}
		tw, th := int(binary.BigEndian.Uint32(p[off:])>>16), int(binary.BigEndian.Uint32(p[off+4:])>>16)
		if tw > 0 && th > 0 && tw <= 16384 && th <= 16384 {
			w, h = tw, th
		}
	})
	return w, h
}

// mp4HasVideo reports whether a track has a video handler.
func mp4HasVideo(data []byte) bool {
	var video bool
	mp4Each(data, []string{"moov", "trak", "mdia", "hdlr"}, func(p []byte) {
		video = video || len(p) >= 12 && string(p[8:12]) == "vide"
	})
	return video
}
