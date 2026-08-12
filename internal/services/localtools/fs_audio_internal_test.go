package localtools

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func wavBytes(n int) []byte {
	data := make([]byte, n*2) // silent samples: all zero
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))    // fmt chunk size
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))     // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))     // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(8000))  // sample rate
	_ = binary.Write(&b, binary.LittleEndian, uint32(16000)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))     // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))    // bits per sample
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func ftypBytes(major string, compatible ...string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(32)) // box size
	b.WriteString("ftyp")
	b.WriteString(major)
	_ = binary.Write(&b, binary.BigEndian, uint32(0x200)) // minor version
	for _, c := range compatible {
		b.WriteString(c)
	}
	for b.Len() < 32 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// Magic bytes decide; extension is only a tiebreak for the two genuinely
// ambiguous cases.
func TestUnit_ClassifyAudio_DetectionTable(t *testing.T) {
	junk := bytes.Repeat([]byte{0x00, 0x7F, 0xE3, 0x11}, 16)
	cases := []struct {
		name   string
		prefix []byte
		ext    string
		mime   string
		det    audioDetection
	}{
		{"wav header", wavBytes(8), ".wav", "audio/wav", supportedAudio},
		{"wav header, lying extension", wavBytes(8), ".txt", "audio/wav", supportedAudio},
		{"mp3 with ID3 tag", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), junk...), ".mp3", "audio/mpeg", supportedAudio},
		{"mp3 with ID3 tag, no extension", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), junk...), "", "audio/mpeg", supportedAudio},
		{"bare mp3 frame sync with .mp3", append([]byte{0xFF, 0xFB, 0x90, 0x64}, junk...), ".mp3", "audio/mpeg", supportedAudio},
		{"bare mp3 frame sync without corroborating extension", append([]byte{0xFF, 0xFB, 0x90, 0x64}, junk...), ".bin", "", notAudio},
		{"jpeg is not an mp3 despite 0xFF lead byte", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, junk...), ".mp3", "", notAudio},
		{"ogg container", append([]byte("OggS\x00\x02"), junk...), ".ogg", "audio/ogg", supportedAudio},
		{"opus in ogg container detects by container magic", append([]byte("OggS\x00\x02"), junk...), ".opus", "audio/ogg", supportedAudio},
		{"flac", append([]byte("fLaC\x00\x00\x00\x22"), junk...), ".flac", "audio/flac", supportedAudio},
		{"m4a brand", ftypBytes("M4A ", "M4A ", "mp42", "isom"), ".m4a", "audio/mp4", supportedAudio},
		{"generic mp4 brand with .m4a extension tiebreak", ftypBytes("isom", "isom", "iso2", "mp41"), ".m4a", "audio/mp4", supportedAudio},
		{"generic mp4 brand with .mp4 extension is video, not audio", ftypBytes("isom", "isom", "iso2", "mp41"), ".mp4", "", notAudio},
		{"aiff is audio but unsupported", append([]byte("FORM\x00\x00\x01\x00AIFF"), junk...), ".aiff", "audio/aiff", unsupportedAudio},
		{"plain text", []byte("package main\n\nfunc main() {}\n"), ".go", "", notAudio},
		{"empty", nil, ".wav", "", notAudio},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, det := classifyAudio(tc.prefix, tc.ext)
			require.Equal(t, tc.det, det, "detection kind")
			require.Equal(t, tc.mime, mime, "canonical mime")
		})
	}
}

func TestUnit_MP3FrameSync_RejectsReservedFields(t *testing.T) {
	// 0xFF 0xE9: sync bits set, but version field == 01 (reserved).
	require.False(t, mp3FrameSync([]byte{0xFF, 0xE9, 0x00, 0x00}))
	// 0xFF 0xF9: valid version (MPEG1), layer field == 00 (reserved).
	require.False(t, mp3FrameSync([]byte{0xFF, 0xF9, 0x00, 0x00}))
	// 0xFF 0xFB: MPEG1 Layer III — the common case.
	require.True(t, mp3FrameSync([]byte{0xFF, 0xFB, 0x90, 0x64}))
}
